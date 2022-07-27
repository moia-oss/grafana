package panelelements

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/grafana/grafana/pkg/bus"
	"github.com/grafana/grafana/pkg/components/simplejson"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/services/libraryelements"
	"github.com/grafana/grafana/pkg/util"
)

var (
	// ErrFolderNameMissing is returned when folder name is missing.
	ErrFolderNameMissing = errors.New("folder name missing")
)

// FileReader is responsible for reading LibraryElementss from disk and
// insert/update LibraryElementss to the Grafana database using
// `LibraryElementss.LibraryElementsProvisioningService`.
type FileReader struct {
	Cfg                                *config
	Path                               string
	log                                log.Logger
	LibraryElementsProvisioningService libraryelements.LibraryElementsProvisioningService
	FoldersFromFilesStructure          bool

	mux                     sync.RWMutex
	usageTracker            *usageTracker
	dbWriteAccessRestricted bool
}

// NewLibraryElementsFileReader returns a new filereader based on `config`
func NewLibraryElementsFileReader(cfg *config, log log.Logger, service libraryelements.LibraryElementsProvisioningService) (*FileReader, error) {
	var path string
	path, ok := cfg.Options["path"].(string)
	if !ok {
		path, ok = cfg.Options["folder"].(string)
		if !ok {
			return nil, fmt.Errorf("failed to load LibraryElementss, path param is not a string")
		}

		log.Warn("[Deprecated] The folder property is deprecated. Please use path instead.")
	}

	foldersFromFilesStructure, _ := cfg.Options["foldersFromFilesStructure"].(bool)
	if foldersFromFilesStructure && cfg.Folder != "" && cfg.FolderUID != "" {
		return nil, fmt.Errorf("'folder' and 'folderUID' should be empty using 'foldersFromFilesStructure' option")
	}

	return &FileReader{
		Cfg:                                cfg,
		Path:                               path,
		log:                                log,
		LibraryElementsProvisioningService: service,
		FoldersFromFilesStructure:          foldersFromFilesStructure,
		usageTracker:                       newUsageTracker(),
	}, nil
}

// pollChanges periodically runs walkDisk based on interval specified in the config.
func (fr *FileReader) pollChanges(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(int64(time.Second) * fr.Cfg.UpdateIntervalSeconds))
	for {
		select {
		case <-ticker.C:
			if err := fr.walkDisk(ctx); err != nil {
				fr.log.Error("failed to search for LibraryElementss", "error", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

// walkDisk traverses the file system for the defined path, reading LibraryElements definition files,
// and applies any change to the database.
func (fr *FileReader) walkDisk(ctx context.Context) error {
	fr.log.Debug("Start walking disk", "path", fr.Path)
	resolvedPath := fr.resolvedPath()
	if _, err := os.Stat(resolvedPath); err != nil {
		return err
	}

	provisionedLibraryElementsRefs, err := getProvisionedLibraryElementssByPath(fr.LibraryElementsProvisioningService, fr.Cfg.Name)
	if err != nil {
		return err
	}

	// Find relevant files
	filesFoundOnDisk := map[string]os.FileInfo{}
	if err := filepath.Walk(resolvedPath, createWalkFn(filesFoundOnDisk)); err != nil {
		return err
	}

	fr.handleMissingLibraryElementsFiles(ctx, provisionedLibraryElementsRefs, filesFoundOnDisk)

	usageTracker := newUsageTracker()
	if fr.FoldersFromFilesStructure {
		err = fr.storeLibraryElementsInFoldersFromFileStructure(ctx, filesFoundOnDisk, provisionedLibraryElementsRefs, resolvedPath, usageTracker)
	} else {
		err = fr.storeLibraryElementsInFolder(ctx, filesFoundOnDisk, provisionedLibraryElementsRefs, usageTracker)
	}
	if err != nil {
		return err
	}

	fr.mux.Lock()
	defer fr.mux.Unlock()

	fr.usageTracker = usageTracker
	return nil
}

func (fr *FileReader) changeWritePermissions(restrict bool) {
	fr.mux.Lock()
	defer fr.mux.Unlock()

	fr.dbWriteAccessRestricted = restrict
}

func (fr *FileReader) isDatabaseAccessRestricted() bool {
	fr.mux.RLock()
	defer fr.mux.RUnlock()

	return fr.dbWriteAccessRestricted
}

// storeLibraryElementssInFolder saves dashboards from the filesystem on disk to the folder from config
func (fr *FileReader) storeDashboardsInFolder(ctx context.Context, filesFoundOnDisk map[string]os.FileInfo,
	dashboardRefs map[string]*models.DashboardProvisioning, usageTracker *usageTracker) error {
	folderID, err := fr.getOrCreateFolderID(ctx, fr.Cfg, fr.dashboardProvisioningService, fr.Cfg.Folder)
	if err != nil && !errors.Is(err, ErrFolderNameMissing) {
		return err
	}

	// save dashboards based on json files
	for path, fileInfo := range filesFoundOnDisk {
		provisioningMetadata, err := fr.saveDashboard(ctx, path, folderID, fileInfo, dashboardRefs)
		if err != nil {
			fr.log.Error("failed to save dashboard", "error", err)
			continue
		}

		usageTracker.track(provisioningMetadata)
	}
	return nil
}

// storeLibraryElementsInFoldersFromFileStructure saves library elements from the filesystem on disk to the same folder
// in Grafana as they are in on the filesystem.
func (fr *FileReader) storeLibraryElementsInFoldersFromFileStructure(ctx context.Context, filesFoundOnDisk map[string]os.FileInfo,
	dashboardRefs map[string]*models.DashboardProvisioning, resolvedPath string, usageTracker *usageTracker) error {
	for path, fileInfo := range filesFoundOnDisk {
		folderName := ""

		LibraryElementsFolder := filepath.Dir(path)
		if LibraryElementsFolder != resolvedPath {
			folderName = filepath.Base(LibraryElementsFolder)
		}

		folderID, err := fr.getOrCreateFolderID(ctx, fr.Cfg, fr.LibraryElementsProvisioningService, folderName)
		if err != nil && !errors.Is(err, ErrFolderNameMissing) {
			return fmt.Errorf("can't provision folder %q from file system structure: %w", folderName, err)
		}

		provisioningMetadata, err := fr.saveLibraryElements(ctx, path, folderID, fileInfo, dashboardRefs)
		usageTracker.track(provisioningMetadata)
		if err != nil {
			fr.log.Error("failed to save library elements", "error", err)
		}
	}
	return nil
}

// handleMissingDashboardFiles will unprovision or delete dashboards which are missing on disk.
func (fr *FileReader) handleMissingLibraryElementsFiles(ctx context.Context, provisionedLibraryElementsRefs map[string]*models.LibraryElementsProvisioning,
	filesFoundOnDisk map[string]os.FileInfo) {
	// find LibraryElementss to delete since json file is missing
	var LibraryElementssToDelete []int64
	for path, provisioningData := range provisionedLibraryElementsRefs {
		_, existsOnDisk := filesFoundOnDisk[path]
		if !existsOnDisk {
			LibraryElementssToDelete = append(LibraryElementssToDelete, provisioningData.LibraryElementsId)
		}
	}

	if fr.Cfg.DisableDeletion {
		// If deletion is disabled for the provisioner we just remove provisioning metadata about the LibraryElements
		// so afterwards the LibraryElements is considered unprovisioned.
		for _, LibraryElementsID := range LibraryElementssToDelete {
			fr.log.Debug("unprovisioning provisioned LibraryElements. missing on disk", "id", LibraryElementsID)
			err := fr.LibraryElementsProvisioningService.UnprovisionLibraryElements(ctx, LibraryElementsID)
			if err != nil {
				fr.log.Error("failed to unprovision LibraryElements", "LibraryElements_id", LibraryElementsID, "error", err)
			}
		}
	} else {
		// delete LibraryElementss missing JSON file
		for _, LibraryElementsID := range LibraryElementssToDelete {
			fr.log.Debug("deleting provisioned LibraryElements, missing on disk", "id", LibraryElementsID)
			err := fr.LibraryElementsProvisioningService.DeleteProvisionedLibraryElements(ctx, LibraryElementsID, fr.Cfg.OrgID)
			if err != nil {
				fr.log.Error("failed to delete LibraryElements", "id", LibraryElementsID, "error", err)
			}
		}
	}
}

// saveLibraryElements saves or updates the LibraryElements provisioning file at path.
func (fr *FileReader) saveLibraryElements(ctx context.Context, path string, folderID int64, fileInfo os.FileInfo,
	provisionedLibraryElementsRefs map[string]*libraryelements.LibraryElementsProvisioning) (provisioningMetadata, error) {
	provisioningMetadata := provisioningMetadata{}
	resolvedFileInfo, err := resolveSymlink(fileInfo, path)
	if err != nil {
		return provisioningMetadata, err
	}

	provisionedData, alreadyProvisioned := provisionedLibraryElementsRefs[path]

	jsonFile, err := fr.readLibraryElementsFromFile(path, resolvedFileInfo.ModTime(), folderID)
	if err != nil {
		fr.log.Error("failed to load LibraryElements from ", "file", path, "error", err)
		return provisioningMetadata, nil
	}

	upToDate := alreadyProvisioned
	if provisionedData != nil {
		upToDate = jsonFile.checkSum == provisionedData.CheckSum
	}

	// keeps track of which UIDs and titles we have already provisioned
	panel := jsonFile.LibraryElements
	provisioningMetadata.uid = panel.LibraryElements.Uid
	provisioningMetadata.identity = LibraryElementsIdentity{title: panel.LibraryElements.Title, folderID: panel.LibraryElements.FolderId}

	if upToDate {
		return provisioningMetadata, nil
	}

	if panel.LibraryElements.Id != 0 {
		panel.LibraryElements.Data.Set("id", nil)
		panel.LibraryElements.Id = 0
	}

	if alreadyProvisioned {
		panel.LibraryElements.SetId(provisionedData.LibraryElementsId)
	}

	if !fr.isDatabaseAccessRestricted() {
		fr.log.Debug("saving new LibraryElements", "provisioner", fr.Cfg.Name, "file", path, "folderId", panel.LibraryElements.FolderId)
		dp := &models.LibraryElementsProvisioning{
			ExternalId: path,
			Name:       fr.Cfg.Name,
			Updated:    resolvedFileInfo.ModTime().Unix(),
			CheckSum:   jsonFile.checkSum,
		}
		_, err := fr.LibraryElementsProvisioningService.SaveProvisionedLibraryElements(ctx, panel, dp)
		if err != nil {
			return provisioningMetadata, err
		}
	} else {
		fr.log.Warn("Not saving new LibraryElements due to restricted database access", "provisioner", fr.Cfg.Name,
			"file", path, "folderId", panel.LibraryElements.FolderId)
	}

	return provisioningMetadata, nil
}

func getProvisionedLibraryElementssByPath(service libraryelements.LibraryElementsProvisioningService, name string) (
	map[string]*libraryelements.LibraryElementsProvisioning, error) {
	arr, err := service.GetProvisionedLibraryElementsData(name)
	if err != nil {
		return nil, err
	}

	byPath := map[string]*libraryelements.LibraryElementsProvisioning{}
	for _, pd := range arr {
		byPath[pd.ExternalID] = pd
	}

	return byPath, nil
}

func (fr *FileReader) getOrCreateFolderID(ctx context.Context, cfg *config, service libraryelements.LibraryElementsProvisioningService, folderName string) (int64, error) {
	if folderName == "" {
		return 0, ErrFolderNameMissing
	}

	cmd := &models.GetLibraryElementsQuery{Slug: models.SlugifyTitle(folderName), OrgId: cfg.OrgID}
	err := bus.Dispatch(ctx, cmd)

	if err != nil && !errors.Is(err, models.ErrLibraryElementsNotFound) {
		return 0, err
	}

	// LibraryElements folder not found. create one.
	if errors.Is(err, models.ErrLibraryElementsNotFound) {
		panel := &libraryelements.SaveLibraryElementDTO{}
		panel.LibraryElement = libraryelements.NewLibraryElementFolder(folderName)
		panel.Overwrite = true
		panel.OrgId = cfg.OrgID
		// set LibraryElements folderUid if given
		panel.LibraryElement.UID = cfg.FolderUID
		dbDash, err := service.SaveFolderForProvisionedLibraryElements(ctx, panel)
		if err != nil {
			return 0, err
		}

		return dbDash.Id, nil
	}

	if !cmd.Result.IsFolder {
		return 0, fmt.Errorf("got invalid response. expected folder, found LibraryElements")
	}

	return cmd.Result.Id, nil
}

func resolveSymlink(fileinfo os.FileInfo, path string) (os.FileInfo, error) {
	checkFilepath, err := filepath.EvalSymlinks(path)
	if path != checkFilepath {
		fi, err := os.Lstat(checkFilepath)
		if err != nil {
			return nil, err
		}

		return fi, nil
	}

	return fileinfo, err
}

func createWalkFn(filesOnDisk map[string]os.FileInfo) filepath.WalkFunc {
	return func(path string, fileInfo os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		isValid, err := validateWalkablePath(fileInfo)
		if !isValid {
			return err
		}

		filesOnDisk[path] = fileInfo
		return nil
	}
}

func validateWalkablePath(fileInfo os.FileInfo) (bool, error) {
	if fileInfo.IsDir() {
		if strings.HasPrefix(fileInfo.Name(), ".") {
			return false, filepath.SkipDir
		}
		return false, nil
	}

	if !strings.HasSuffix(fileInfo.Name(), ".json") {
		return false, nil
	}

	return true, nil
}

type LibraryElementsJSONFile struct {
	LibraryElements *libraryelements.SaveLibraryElementDTO
	checkSum        string
	lastModified    time.Time
}

func (fr *FileReader) readLibraryElementsFromFile(path string, lastModified time.Time, folderID int64) (*LibraryElementsJSONFile, error) {
	// nolint:gosec
	// We can ignore the gosec G304 warning on this one because `path` comes from the provisioning configuration file.
	reader, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := reader.Close(); err != nil {
			fr.log.Warn("Failed to close file", "path", path, "err", err)
		}
	}()

	all, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	checkSum, err := util.Md5SumString(string(all))
	if err != nil {
		return nil, err
	}

	data, err := simplejson.NewJson(all)
	if err != nil {
		return nil, err
	}

	dash, err := createLibraryElementsJSON(data, lastModified, fr.Cfg, folderID)
	if err != nil {
		return nil, err
	}

	return &LibraryElementsJSONFile{
		LibraryElements: dash,
		checkSum:        checkSum,
		lastModified:    lastModified,
	}, nil
}

func (fr *FileReader) resolvedPath() string {
	if _, err := os.Stat(fr.Path); os.IsNotExist(err) {
		fr.log.Error("Cannot read directory", "error", err)
	}

	path, err := filepath.Abs(fr.Path)
	if err != nil {
		fr.log.Error("Could not create absolute path", "path", fr.Path, "error", err)
	}

	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		fr.log.Error("Failed to read content of symlinked path", "path", fr.Path, "error", err)
	}

	if path == "" {
		path = fr.Path
		fr.log.Info("falling back to original path due to EvalSymlink/Abs failure")
	}
	return path
}

func (fr *FileReader) getUsageTracker() *usageTracker {
	fr.mux.RLock()
	defer fr.mux.RUnlock()

	return fr.usageTracker
}

type provisioningMetadata struct {
	uid      string
	identity LibraryElementsIdentity
}

type LibraryElementsIdentity struct {
	folderID int64
	title    string
}

func (d *LibraryElementsIdentity) Exists() bool {
	return len(d.title) > 0
}

func newUsageTracker() *usageTracker {
	return &usageTracker{
		uidUsage:   map[string]uint8{},
		titleUsage: map[LibraryElementsIdentity]uint8{},
	}
}

type usageTracker struct {
	uidUsage   map[string]uint8
	titleUsage map[LibraryElementsIdentity]uint8
}

func (t *usageTracker) track(pm provisioningMetadata) {
	if len(pm.uid) > 0 {
		t.uidUsage[pm.uid]++
	}
	if pm.identity.Exists() {
		t.titleUsage[pm.identity]++
	}
}
