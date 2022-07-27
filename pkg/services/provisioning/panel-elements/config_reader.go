package panelelements

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/services/provisioning/utils"
	"gopkg.in/yaml.v2"
)

type configReader struct {
	path     string
	log      log.Logger
	orgStore utils.OrgStore
}

func (cr *configReader) parseConfigs(file os.FileInfo) ([]*config, error) {
	filename, _ := filepath.Abs(filepath.Join(cr.path, file.Name()))

	// nolint:gosec
	// We can ignore the gosec G304 warning on this one because `filename` comes from ps.Cfg.ProvisioningPath
	yamlFile, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	apiVersion := &configVersion{APIVersion: 0}

	// We ignore the error here because it errors out for version 0 which does not have apiVersion
	// specified (so 0 is default). This can also error in case the apiVersion is not an integer but at the moment
	// this does not handle that case and would still go on as if version = 0.
	// TODO: return appropriate error in case the apiVersion is specified but isn't integer (or even if it is
	//  integer > max version?).
	_ = yaml.Unmarshal(yamlFile, &apiVersion)

	if apiVersion.APIVersion > 0 {
		v1 := &configV0{}
		err := yaml.Unmarshal(yamlFile, &v1)
		if err != nil {
			return nil, err
		}

		if v1 != nil {
			return v1.mapToPanelElementsAsConfig()
		}
	}

	return []*config{}, nil
}

func (cr *configReader) readConfig(ctx context.Context) ([]*config, error) {
	var panels []*config

	files, err := ioutil.ReadDir(cr.path)
	if err != nil {
		cr.log.Error("can't read dashboard provisioning files from directory", "path", cr.path, "error", err)
		return panels, nil
	}

	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".yaml") && !strings.HasSuffix(file.Name(), ".yml") {
			continue
		}

		parsedPanels, err := cr.parseConfigs(file)
		if err != nil {
			return nil, fmt.Errorf("could not parse provisioning config file: %s error: %v", file.Name(), err)
		}

		if len(parsedPanels) > 0 {
			panels = append(panels, parsedPanels...)
		}
	}

	uidUsage := map[string]uint8{}
	for _, panel := range panels {
		if panel.OrgID == 0 {
			panel.OrgID = 1
		}

		if err := utils.CheckOrgExists(ctx, cr.orgStore, panel.OrgID); err != nil {
			return nil, fmt.Errorf("failed to provision panel with %q reader: %w", panel.Name, err)
		}

		if panel.Type == "" {
			panel.Type = "file"
		}

		if panel.UpdateIntervalSeconds == 0 {
			panel.UpdateIntervalSeconds = 10
		}
		if len(panel.FolderUID) > 0 {
			uidUsage[panel.FolderUID]++
		}
	}

	for uid, times := range uidUsage {
		if times > 1 {
			cr.log.Error("the same folder UID is used more than once", "folderUid", uid)
		}
	}

	return panels, nil
}
