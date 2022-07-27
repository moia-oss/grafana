package panelelements

import (
	"fmt"
	"time"

	"github.com/grafana/grafana/pkg/components/simplejson"
	"github.com/grafana/grafana/pkg/services/libraryelements"
	"github.com/grafana/grafana/pkg/services/provisioning/values"
)

type config struct {
	Name                  string
	Type                  string
	OrgID                 int64
	Folder                string
	FolderUID             string
	Editable              bool
	Options               map[string]interface{}
	DisableDeletion       bool
	UpdateIntervalSeconds int64
	AllowUIUpdates        bool
}

type configV0 struct {
	Providers []*configs `json:"providers" yaml:"providers"`
}

type configVersion struct {
	APIVersion int64 `json:"apiVersion" yaml:"apiVersion"`
}

type configs struct {
	Name                  values.StringValue `json:"name" yaml:"name"`
	Type                  values.StringValue `json:"type" yaml:"type"`
	OrgID                 values.Int64Value  `json:"orgId" yaml:"orgId"`
	Folder                values.StringValue `json:"folder" yaml:"folder"`
	FolderUID             values.StringValue `json:"folderUid" yaml:"folderUid"`
	Editable              values.BoolValue   `json:"editable" yaml:"editable"`
	Options               values.JSONValue   `json:"options" yaml:"options"`
	DisableDeletion       values.BoolValue   `json:"disableDeletion" yaml:"disableDeletion"`
	UpdateIntervalSeconds values.Int64Value  `json:"updateIntervalSeconds" yaml:"updateIntervalSeconds"`
	AllowUIUpdates        values.BoolValue   `json:"allowUiUpdates" yaml:"allowUiUpdates"`
}

func createPanelElementsJSON(data *simplejson.Json, lastModified time.Time, cfg *config, folderID int64) (*libraryelements.SaveLibraryElementDTO, error) {
	panel := &libraryelements.SaveLibraryElementDTO{}
	panel.LibraryElement = libraryelements.NewLibraryElementFromJson(data)
	panel.UpdatedAt = lastModified
	panel.Overwrite = true
	panel.OrgId = cfg.OrgID
	panel.LibraryElement.OrgID = cfg.OrgID
	panel.LibraryElement.FolderID = folderID

	return panel, nil
}

func (dc *configV0) mapToPanelElementsAsConfig() ([]*config, error) {
	var r []*config
	seen := make(map[string]bool)

	for _, v := range dc.Providers {
		if _, ok := seen[v.Name.Value()]; ok {
			return nil, fmt.Errorf("PanelElements name %q is not unique", v.Name.Value())
		}
		seen[v.Name.Value()] = true

		r = append(r, &config{
			Name:                  v.Name.Value(),
			Type:                  v.Type.Value(),
			OrgID:                 v.OrgID.Value(),
			Folder:                v.Folder.Value(),
			FolderUID:             v.FolderUID.Value(),
			Editable:              v.Editable.Value(),
			Options:               v.Options.Value(),
			DisableDeletion:       v.DisableDeletion.Value(),
			UpdateIntervalSeconds: v.UpdateIntervalSeconds.Value(),
			AllowUIUpdates:        v.AllowUIUpdates.Value(),
		})
	}

	return r, nil
}
