package service

import (
	"context"
	"fmt"

	"github.com/grafana/grafana/pkg/bus"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/plugins"
	"github.com/grafana/grafana/pkg/services/dashboardimport"
	"github.com/grafana/grafana/pkg/services/dashboards"
	"github.com/grafana/grafana/pkg/services/plugindashboards"
	"github.com/grafana/grafana/pkg/services/pluginsettings"
)

func ProvideDashboardUpdater(bus bus.Bus, pluginStore plugins.Store, pluginDashboardService plugindashboards.Service,
	dashboardImportService dashboardimport.Service, pluginSettingsService pluginsettings.Service,
	dashboardPluginService dashboards.PluginService, dashboardService dashboards.DashboardService) *DashboardUpdater {
	du := newDashboardUpdater(bus, pluginStore, pluginDashboardService, dashboardImportService,
		pluginSettingsService, dashboardPluginService, dashboardService)
	du.updateAppDashboards()
	return du
}

func newDashboardUpdater(bus bus.Bus, pluginStore plugins.Store,
	pluginDashboardService plugindashboards.Service, dashboardImportService dashboardimport.Service,
	pluginSettingsService pluginsettings.Service, dashboardPluginService dashboards.PluginService,
	dashboardService dashboards.DashboardService) *DashboardUpdater {
	s := &DashboardUpdater{
		pluginStore:            pluginStore,
		pluginDashboardService: pluginDashboardService,
		dashboardImportService: dashboardImportService,
		pluginSettingsService:  pluginSettingsService,
		dashboardPluginService: dashboardPluginService,
		dashboardService:       dashboardService,
		logger:                 log.New("plugindashboards"),
	}
	bus.AddEventListener(s.handlePluginStateChanged)

	return s
}

type DashboardUpdater struct {
	pluginStore            plugins.Store
	pluginDashboardService plugindashboards.Service
	dashboardImportService dashboardimport.Service
	pluginSettingsService  pluginsettings.Service
	dashboardPluginService dashboards.PluginService
	dashboardService       dashboards.DashboardService
	logger                 log.Logger
}

func (du *DashboardUpdater) updateAppDashboards() {
	du.logger.Debug("Looking for app dashboard updates")

	pluginSettings, err := du.pluginSettingsService.GetPluginSettings(context.Background(), 0)
	if err != nil {
		du.logger.Error("Failed to get all plugin settings", "error", err)
		return
	}

	for _, pluginSetting := range pluginSettings {
		// ignore disabled plugins
		if !pluginSetting.Enabled {
			continue
		}

		if pluginDef, exists := du.pluginStore.Plugin(context.Background(), pluginSetting.PluginId); exists {
			if pluginDef.Info.Version != pluginSetting.PluginVersion {
				du.syncPluginDashboards(context.Background(), pluginDef, pluginSetting.OrgId)
			}
		}
	}
}

func (du *DashboardUpdater) syncPluginDashboards(ctx context.Context, plugin plugins.PluginDTO, orgID int64) {
	du.logger.Info("Syncing plugin dashboards to DB", "pluginId", plugin.ID)

	// Get plugin dashboards
	req := &plugindashboards.ListPluginDashboardsRequest{
		OrgID:    orgID,
		PluginID: plugin.ID,
	}
	resp, err := du.pluginDashboardService.ListPluginDashboards(ctx, req)
	if err != nil {
		du.logger.Error("Failed to load app dashboards", "error", err)
		return
	}

	// Update dashboards with updated revisions
	for _, dash := range resp.Items {
		// remove removed ones
		if dash.Removed {
			du.logger.Info("Deleting plugin dashboard", "pluginId", plugin.ID, "dashboard", dash.Slug)

			if err := du.dashboardService.DeleteDashboard(ctx, dash.DashboardId, orgID); err != nil {
				du.logger.Error("Failed to auto update app dashboard", "pluginId", plugin.ID, "error", err)
				return
			}

			continue
		}

		// update updated ones
		if dash.ImportedRevision != dash.Revision {
			if err := du.autoUpdateAppDashboard(ctx, dash, orgID); err != nil {
				du.logger.Error("Failed to auto update app dashboard", "pluginId", plugin.ID, "error", err)
				return
			}
		}
	}

	// update version in plugin_setting table to mark that we have processed the update
	query := models.GetPluginSettingByIdQuery{PluginId: plugin.ID, OrgId: orgID}
	if err := du.pluginSettingsService.GetPluginSettingById(ctx, &query); err != nil {
		du.logger.Error("Failed to read plugin setting by ID", "error", err)
		return
	}

	appSetting := query.Result
	cmd := models.UpdatePluginSettingVersionCmd{
		OrgId:         appSetting.OrgId,
		PluginId:      appSetting.PluginId,
		PluginVersion: plugin.Info.Version,
	}

	if err := du.pluginSettingsService.UpdatePluginSettingVersion(ctx, &cmd); err != nil {
		du.logger.Error("Failed to update plugin setting version", "error", err)
	}
}

func (du *DashboardUpdater) handlePluginStateChanged(ctx context.Context, event *models.PluginStateChangedEvent) error {
	du.logger.Info("Plugin state changed", "pluginId", event.PluginId, "enabled", event.Enabled)

	if event.Enabled {
		p, exists := du.pluginStore.Plugin(ctx, event.PluginId)
		if !exists {
			return fmt.Errorf("plugin %s not found. Could not sync plugin dashboards", event.PluginId)
		}

		du.syncPluginDashboards(ctx, p, event.OrgId)
	} else {
		query := models.GetDashboardsByPluginIdQuery{PluginId: event.PluginId, OrgId: event.OrgId}
		if err := du.dashboardPluginService.GetDashboardsByPluginID(ctx, &query); err != nil {
			return err
		}

		for _, dash := range query.Result {
			du.logger.Info("Deleting plugin dashboard", "pluginId", event.PluginId, "dashboard", dash.Slug)
			if err := du.dashboardService.DeleteDashboard(ctx, dash.Id, dash.OrgId); err != nil {
				return err
			}
		}
	}

	return nil
}

func (du *DashboardUpdater) autoUpdateAppDashboard(ctx context.Context, pluginDashInfo *plugindashboards.PluginDashboard, orgID int64) error {
	req := &plugindashboards.LoadPluginDashboardRequest{
		PluginID:  pluginDashInfo.PluginId,
		Reference: pluginDashInfo.Reference,
	}
	resp, err := du.pluginDashboardService.LoadPluginDashboard(ctx, req)
	if err != nil {
		return err
	}
	du.logger.Info("Auto updating App dashboard", "dashboard", resp.Dashboard.Title, "newRev",
		pluginDashInfo.Revision, "oldRev", pluginDashInfo.ImportedRevision)
	_, err = du.dashboardImportService.ImportDashboard(ctx, &dashboardimport.ImportDashboardRequest{
		PluginId:  pluginDashInfo.PluginId,
		User:      &models.SignedInUser{UserId: 0, OrgRole: models.ROLE_ADMIN, OrgId: orgID},
		Path:      pluginDashInfo.Reference,
		FolderId:  0,
		Dashboard: resp.Dashboard.Data,
		Overwrite: true,
		Inputs:    nil,
	})
	return err
}
