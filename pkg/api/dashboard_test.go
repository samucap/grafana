package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/grafana/grafana/pkg/api/dtos"
	"github.com/grafana/grafana/pkg/api/response"
	"github.com/grafana/grafana/pkg/api/routing"
	"github.com/grafana/grafana/pkg/bus"
	"github.com/grafana/grafana/pkg/components/simplejson"
	"github.com/grafana/grafana/pkg/infra/db"
	"github.com/grafana/grafana/pkg/infra/db/dbtest"
	"github.com/grafana/grafana/pkg/infra/localcache"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/infra/tracing"
	"github.com/grafana/grafana/pkg/infra/usagestats"
	"github.com/grafana/grafana/pkg/registry/corekind"
	"github.com/grafana/grafana/pkg/services/accesscontrol"
	"github.com/grafana/grafana/pkg/services/accesscontrol/acimpl"
	"github.com/grafana/grafana/pkg/services/accesscontrol/actest"
	accesscontrolmock "github.com/grafana/grafana/pkg/services/accesscontrol/mock"
	"github.com/grafana/grafana/pkg/services/alerting"
	"github.com/grafana/grafana/pkg/services/annotations/annotationstest"
	"github.com/grafana/grafana/pkg/services/auth/identity"
	contextmodel "github.com/grafana/grafana/pkg/services/contexthandler/model"
	"github.com/grafana/grafana/pkg/services/dashboards"
	"github.com/grafana/grafana/pkg/services/dashboards/database"
	"github.com/grafana/grafana/pkg/services/dashboards/service"
	dashver "github.com/grafana/grafana/pkg/services/dashboardversion"
	"github.com/grafana/grafana/pkg/services/dashboardversion/dashvertest"
	"github.com/grafana/grafana/pkg/services/featuremgmt"
	"github.com/grafana/grafana/pkg/services/folder"
	"github.com/grafana/grafana/pkg/services/folder/folderimpl"
	"github.com/grafana/grafana/pkg/services/folder/foldertest"
	"github.com/grafana/grafana/pkg/services/guardian"
	"github.com/grafana/grafana/pkg/services/libraryelements/model"
	"github.com/grafana/grafana/pkg/services/librarypanels"
	"github.com/grafana/grafana/pkg/services/live"
	"github.com/grafana/grafana/pkg/services/org"
	"github.com/grafana/grafana/pkg/services/pluginsintegration/pluginstore"
	pref "github.com/grafana/grafana/pkg/services/preference"
	"github.com/grafana/grafana/pkg/services/preference/preftest"
	"github.com/grafana/grafana/pkg/services/provisioning"
	"github.com/grafana/grafana/pkg/services/publicdashboards"
	"github.com/grafana/grafana/pkg/services/publicdashboards/api"
	"github.com/grafana/grafana/pkg/services/quota/quotatest"
	"github.com/grafana/grafana/pkg/services/star/startest"
	"github.com/grafana/grafana/pkg/services/tag/tagimpl"
	"github.com/grafana/grafana/pkg/services/user"
	"github.com/grafana/grafana/pkg/services/user/usertest"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/web"
	"github.com/grafana/grafana/pkg/web/webtest"
)

func TestGetHomeDashboard(t *testing.T) {
	httpReq, err := http.NewRequest(http.MethodGet, "", nil)
	require.NoError(t, err)
	httpReq.Header.Add("Content-Type", "application/json")
	req := &contextmodel.ReqContext{SignedInUser: &user.SignedInUser{}, Context: &web.Context{Req: httpReq}}
	cfg := setting.NewCfg()
	cfg.StaticRootPath = "../../public/"
	prefService := preftest.NewPreferenceServiceFake()
	dashboardVersionService := dashvertest.NewDashboardVersionServiceFake()

	hs := &HTTPServer{
		Cfg:                     cfg,
		pluginStore:             &pluginstore.FakePluginStore{},
		SQLStore:                dbtest.NewFakeDB(),
		preferenceService:       prefService,
		dashboardVersionService: dashboardVersionService,
		Kinds:                   corekind.NewBase(nil),
		log:                     log.New("test-logger"),
	}

	tests := []struct {
		name                  string
		defaultSetting        string
		expectedDashboardPath string
	}{
		{name: "using default config", defaultSetting: "", expectedDashboardPath: "../../public/dashboards/home.json"},
		{name: "custom path", defaultSetting: "../../public/dashboards/default.json", expectedDashboardPath: "../../public/dashboards/default.json"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dash := dtos.DashboardFullWithMeta{}
			dash.Meta.FolderTitle = "General"

			homeDashJSON, err := os.ReadFile(tc.expectedDashboardPath)
			require.NoError(t, err, "must be able to read expected dashboard file")
			hs.Cfg.DefaultHomeDashboardPath = tc.defaultSetting
			bytes, err := simplejson.NewJson(homeDashJSON)
			require.NoError(t, err, "must be able to encode file as JSON")

			prefService.ExpectedPreference = &pref.Preference{}

			dash.Dashboard = bytes

			b, err := json.Marshal(dash)
			require.NoError(t, err, "must be able to marshal object to JSON")

			res := hs.GetHomeDashboard(req)
			nr, ok := res.(*response.NormalResponse)
			require.True(t, ok, "should return *NormalResponse")
			require.Equal(t, b, nr.Body(), "default home dashboard should equal content on disk")
		})
	}
}

func newTestLive(t *testing.T, store db.DB) *live.GrafanaLive {
	features := featuremgmt.WithFeatures()
	cfg := setting.NewCfg()
	cfg.AppURL = "http://localhost:3000/"
	gLive, err := live.ProvideService(nil, cfg,
		routing.NewRouteRegister(),
		nil, nil, nil, nil,
		store,
		nil,
		&usagestats.UsageStatsMock{T: t},
		nil,
		features, acimpl.ProvideAccessControl(cfg), &dashboards.FakeDashboardService{}, annotationstest.NewFakeAnnotationsRepo(), nil)
	require.NoError(t, err)
	return gLive
}

func TestHTTPServer_GetDashboard_AccessControl(t *testing.T) {
	setup := func() *webtest.Server {
		return SetupAPITestServer(t, func(hs *HTTPServer) {
			dash := dashboards.NewDashboard("some dash")
			dash.ID = 1
			dash.UID = "1"

			dashSvc := dashboards.NewFakeDashboardService(t)
			dashSvc.On("GetDashboard", mock.Anything, mock.Anything).Return(dash, nil).Maybe()
			hs.DashboardService = dashSvc

			hs.Cfg = setting.NewCfg()
			hs.AccessControl = acimpl.ProvideAccessControl(hs.Cfg)
			hs.starService = startest.NewStarServiceFake()
			hs.dashboardProvisioningService = mockDashboardProvisioningService{}

			guardian.InitAccessControlGuardian(hs.Cfg, hs.AccessControl, hs.DashboardService)
		})
	}

	getDashboard := func(server *webtest.Server, permissions []accesscontrol.Permission) (*http.Response, error) {
		return server.Send(webtest.RequestWithSignedInUser(server.NewGetRequest("/api/dashboards/uid/1"), userWithPermissions(1, permissions)))
	}

	t.Run("Should not be able to get dashboard without correct permission", func(t *testing.T) {
		server := setup()

		res, err := getDashboard(server, nil)
		require.NoError(t, err)

		assert.Equal(t, http.StatusForbidden, res.StatusCode)
		require.NoError(t, res.Body.Close())
	})

	t.Run("Should be able to get when user has permission to read dashboard", func(t *testing.T) {
		server := setup()

		permissions := []accesscontrol.Permission{{Action: dashboards.ActionDashboardsRead, Scope: "dashboards:uid:1"}}
		res, err := getDashboard(server, permissions)
		require.NoError(t, err)

		assert.Equal(t, http.StatusOK, res.StatusCode)
		var data dtos.DashboardFullWithMeta
		require.NoError(t, json.NewDecoder(res.Body).Decode(&data))

		assert.Equal(t, data.Meta.CanSave, false)
		assert.Equal(t, data.Meta.CanEdit, false)
		assert.Equal(t, data.Meta.CanDelete, false)
		assert.Equal(t, data.Meta.CanAdmin, false)

		require.NoError(t, res.Body.Close())
	})

	t.Run("Should set CanSave and CanEdit with correct permissions", func(t *testing.T) {
		server := setup()

		res, err := getDashboard(server, []accesscontrol.Permission{
			{Action: dashboards.ActionDashboardsRead, Scope: "dashboards:uid:1"},
			{Action: dashboards.ActionDashboardsWrite, Scope: "dashboards:uid:1"},
		})
		require.NoError(t, err)

		assert.Equal(t, http.StatusOK, res.StatusCode)
		var data dtos.DashboardFullWithMeta
		require.NoError(t, json.NewDecoder(res.Body).Decode(&data))

		assert.Equal(t, data.Meta.CanSave, true)
		assert.Equal(t, data.Meta.CanEdit, true)
		assert.Equal(t, data.Meta.CanDelete, false)
		assert.Equal(t, data.Meta.CanAdmin, false)

		require.NoError(t, res.Body.Close())
	})

	t.Run("Should set canDelete with correct permissions", func(t *testing.T) {
		server := setup()

		res, err := getDashboard(server, []accesscontrol.Permission{
			{Action: dashboards.ActionDashboardsRead, Scope: "dashboards:uid:1"},
			{Action: dashboards.ActionDashboardsDelete, Scope: "dashboards:uid:1"},
		})
		require.NoError(t, err)

		assert.Equal(t, http.StatusOK, res.StatusCode)
		var data dtos.DashboardFullWithMeta
		require.NoError(t, json.NewDecoder(res.Body).Decode(&data))

		assert.Equal(t, data.Meta.CanSave, false)
		assert.Equal(t, data.Meta.CanEdit, false)
		assert.Equal(t, data.Meta.CanDelete, true)
		assert.Equal(t, data.Meta.CanAdmin, false)

		require.NoError(t, res.Body.Close())
	})

	t.Run("Should set canAdmin with correct permissions", func(t *testing.T) {
		server := setup()

		res, err := getDashboard(server, []accesscontrol.Permission{
			{Action: dashboards.ActionDashboardsRead, Scope: "dashboards:uid:1"},
			{Action: dashboards.ActionDashboardsPermissionsRead, Scope: "dashboards:uid:1"},
			{Action: dashboards.ActionDashboardsPermissionsWrite, Scope: "dashboards:uid:1"},
		})
		require.NoError(t, err)

		assert.Equal(t, http.StatusOK, res.StatusCode)
		var data dtos.DashboardFullWithMeta
		require.NoError(t, json.NewDecoder(res.Body).Decode(&data))

		assert.Equal(t, data.Meta.CanSave, false)
		assert.Equal(t, data.Meta.CanEdit, false)
		assert.Equal(t, data.Meta.CanDelete, false)
		assert.Equal(t, data.Meta.CanAdmin, true)

		require.NoError(t, res.Body.Close())
	})
}

func TestHTTPServer_DeleteDashboardByUID_AccessControl(t *testing.T) {
	setup := func() *webtest.Server {
		return SetupAPITestServer(t, func(hs *HTTPServer) {
			dash := dashboards.NewDashboard("some dash")
			dash.ID = 1
			dash.UID = "1"

			dashSvc := dashboards.NewFakeDashboardService(t)
			dashSvc.On("GetDashboard", mock.Anything, mock.Anything).Return(dash, nil).Maybe()
			dashSvc.On("DeleteDashboard", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
			hs.DashboardService = dashSvc

			hs.Cfg = setting.NewCfg()
			hs.AccessControl = acimpl.ProvideAccessControl(hs.Cfg)
			hs.starService = startest.NewStarServiceFake()

			hs.LibraryPanelService = &mockLibraryPanelService{}
			hs.LibraryElementService = &mockLibraryElementService{}

			pubDashService := publicdashboards.NewFakePublicDashboardService(t)
			pubDashService.On("DeleteByDashboard", mock.Anything, mock.Anything).Return(nil).Maybe()
			hs.PublicDashboardsApi = api.ProvideApi(pubDashService, nil, hs.AccessControl, featuremgmt.WithFeatures())

			guardian.InitAccessControlGuardian(hs.Cfg, hs.AccessControl, hs.DashboardService)
		})
	}
	deleteDashboard := func(server *webtest.Server, permissions []accesscontrol.Permission) (*http.Response, error) {
		return server.Send(webtest.RequestWithSignedInUser(server.NewRequest(http.MethodDelete, "/api/dashboards/uid/1", nil), userWithPermissions(1, permissions)))
	}

	t.Run("Should not be able to delete dashboard without correct permission", func(t *testing.T) {
		server := setup()
		res, err := deleteDashboard(server, []accesscontrol.Permission{
			{Action: dashboards.ActionDashboardsDelete, Scope: "dashboards:uid:2"},
		})
		require.NoError(t, err)

		assert.Equal(t, http.StatusForbidden, res.StatusCode)
		require.NoError(t, res.Body.Close())
	})

	t.Run("Should be able to delete dashboard with correct permission", func(t *testing.T) {
		server := setup()
		res, err := deleteDashboard(server, []accesscontrol.Permission{
			{Action: dashboards.ActionDashboardsDelete, Scope: "dashboards:uid:1"},
		})
		require.NoError(t, err)

		assert.Equal(t, http.StatusOK, res.StatusCode)
		require.NoError(t, res.Body.Close())
	})
}

func TestHTTPServer_GetDashboardVersions_AccessControl(t *testing.T) {
	setup := func() *webtest.Server {
		return SetupAPITestServer(t, func(hs *HTTPServer) {
			dash := dashboards.NewDashboard("some dash")
			dash.ID = 1
			dash.UID = "1"

			dashSvc := dashboards.NewFakeDashboardService(t)
			dashSvc.On("GetDashboard", mock.Anything, mock.Anything).Return(dash, nil).Maybe()
			dashSvc.On("DeleteDashboard", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
			hs.DashboardService = dashSvc

			hs.Cfg = setting.NewCfg()
			hs.AccessControl = acimpl.ProvideAccessControl(hs.Cfg)
			hs.starService = startest.NewStarServiceFake()

			hs.dashboardVersionService = &dashvertest.FakeDashboardVersionService{
				ExpectedListDashboarVersions: []*dashver.DashboardVersionDTO{},
				ExpectedDashboardVersion:     &dashver.DashboardVersionDTO{},
			}

			guardian.InitAccessControlGuardian(hs.Cfg, hs.AccessControl, hs.DashboardService)
		})
	}

	getVersion := func(server *webtest.Server, permissions []accesscontrol.Permission) (*http.Response, error) {
		return server.Send(webtest.RequestWithSignedInUser(server.NewGetRequest("/api/dashboards/uid/1/versions/1"), userWithPermissions(1, permissions)))
	}

	getVersions := func(server *webtest.Server, permissions []accesscontrol.Permission) (*http.Response, error) {
		return server.Send(webtest.RequestWithSignedInUser(server.NewGetRequest("/api/dashboards/uid/1/versions"), userWithPermissions(1, permissions)))
	}

	t.Run("Should not be able to list dashboard versions without correct permission", func(t *testing.T) {
		server := setup()

		res, err := getVersions(server, []accesscontrol.Permission{})
		require.NoError(t, err)
		assert.Equal(t, http.StatusForbidden, res.StatusCode)
		require.NoError(t, res.Body.Close())

		res, err = getVersion(server, []accesscontrol.Permission{})
		require.NoError(t, err)
		assert.Equal(t, http.StatusForbidden, res.StatusCode)

		require.NoError(t, res.Body.Close())
	})

	t.Run("Should be able to list dashboard versions with correct permission", func(t *testing.T) {
		server := setup()

		permissions := []accesscontrol.Permission{
			{Action: dashboards.ActionDashboardsRead, Scope: "dashboards:uid:1"},
			{Action: dashboards.ActionDashboardsWrite, Scope: "dashboards:uid:1"},
		}

		res, err := getVersions(server, permissions)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, res.StatusCode)
		require.NoError(t, res.Body.Close())

		res, err = getVersion(server, permissions)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, res.StatusCode)

		require.NoError(t, res.Body.Close())
	})
}

func TestDashboardAPIEndpoint(t *testing.T) {
	t.Run("Given two dashboards with the same title in different folders", func(t *testing.T) {
		dashOne := dashboards.NewDashboard("dash")
		dashOne.ID = 2
		// nolint:staticcheck
		dashOne.FolderID = 1
		dashOne.HasACL = false

		dashTwo := dashboards.NewDashboard("dash")
		dashTwo.ID = 4
		// nolint:staticcheck
		dashTwo.FolderID = 3
		dashTwo.HasACL = false
	})

	t.Run("Post dashboard response tests", func(t *testing.T) {
		dashboardStore := &dashboards.FakeDashboardStore{}
		defer dashboardStore.AssertExpectations(t)
		// This tests that a valid request returns correct response
		t.Run("Given a correct request for creating a dashboard", func(t *testing.T) {
			const folderID int64 = 3
			folderUID := "Folder"
			const dashID int64 = 2

			cmd := dashboards.SaveDashboardCommand{
				OrgID:  1,
				UserID: 5,
				Dashboard: simplejson.NewFromAny(map[string]any{
					"title": "Dash",
				}),
				Overwrite: true,
				FolderID:  folderID, // nolint:staticcheck
				FolderUID: folderUID,
				IsFolder:  false,
				Message:   "msg",
			}

			dashboardService := dashboards.NewFakeDashboardService(t)
			// nolint:staticcheck
			dashboardService.On("SaveDashboard", mock.Anything, mock.AnythingOfType("*dashboards.SaveDashboardDTO"), mock.AnythingOfType("bool")).
				Return(&dashboards.Dashboard{ID: dashID, UID: "uid", Title: "Dash", Slug: "dash", Version: 2, FolderUID: folderUID, FolderID: folderID}, nil)
			mockFolderService := &foldertest.FakeService{
				ExpectedFolder: &folder.Folder{ID: 1, UID: folderUID, Title: "Folder"},
			}

			postDashboardScenario(t, "When calling POST on", "/api/dashboards", "/api/dashboards", cmd, dashboardService, mockFolderService, func(sc *scenarioContext) {
				callPostDashboardShouldReturnSuccess(sc)

				result := sc.ToJSON()
				assert.Equal(t, "success", result.Get("status").MustString())
				assert.Equal(t, dashID, result.Get("id").MustInt64())
				assert.Equal(t, "uid", result.Get("uid").MustString())
				assert.Equal(t, "dash", result.Get("slug").MustString())
				assert.Equal(t, "/d/uid/dash", result.Get("url").MustString())
			})
		})

		t.Run("Given a correct request for creating a dashboard with folder uid", func(t *testing.T) {
			const folderUid string = "folderUID"
			const dashID int64 = 2

			cmd := dashboards.SaveDashboardCommand{
				OrgID:  1,
				UserID: 5,
				Dashboard: simplejson.NewFromAny(map[string]any{
					"title": "Dash",
				}),
				Overwrite: true,
				FolderUID: folderUid,
				IsFolder:  false,
				Message:   "msg",
			}

			dashboardService := dashboards.NewFakeDashboardService(t)
			dashboardService.On("SaveDashboard", mock.Anything, mock.AnythingOfType("*dashboards.SaveDashboardDTO"), mock.AnythingOfType("bool")).
				Return(&dashboards.Dashboard{ID: dashID, UID: "uid", Title: "Dash", Slug: "dash", Version: 2}, nil)

			mockFolder := &foldertest.FakeService{
				ExpectedFolder: &folder.Folder{ID: 1, UID: "folderUID", Title: "Folder"},
			}

			postDashboardScenario(t, "When calling POST on", "/api/dashboards", "/api/dashboards", cmd, dashboardService, mockFolder, func(sc *scenarioContext) {
				callPostDashboardShouldReturnSuccess(sc)

				result := sc.ToJSON()
				assert.Equal(t, "success", result.Get("status").MustString())
				assert.Equal(t, dashID, result.Get("id").MustInt64())
				assert.Equal(t, "uid", result.Get("uid").MustString())
				assert.Equal(t, "dash", result.Get("slug").MustString())
				assert.Equal(t, "/d/uid/dash", result.Get("url").MustString())
			})
		})

		// This tests that invalid requests returns expected error responses
		t.Run("Given incorrect requests for creating a dashboard", func(t *testing.T) {
			testCases := []struct {
				SaveError          error
				ExpectedStatusCode int
			}{
				{SaveError: dashboards.ErrDashboardNotFound, ExpectedStatusCode: http.StatusNotFound},
				{SaveError: dashboards.ErrFolderNotFound, ExpectedStatusCode: http.StatusBadRequest},
				{SaveError: dashboards.ErrDashboardWithSameUIDExists, ExpectedStatusCode: http.StatusBadRequest},
				{SaveError: dashboards.ErrDashboardWithSameNameInFolderExists, ExpectedStatusCode: http.StatusPreconditionFailed},
				{SaveError: dashboards.ErrDashboardVersionMismatch, ExpectedStatusCode: http.StatusPreconditionFailed},
				{SaveError: dashboards.ErrDashboardTitleEmpty, ExpectedStatusCode: http.StatusBadRequest},
				{SaveError: dashboards.ErrDashboardFolderCannotHaveParent, ExpectedStatusCode: http.StatusBadRequest},
				{SaveError: alerting.ValidationError{Reason: "Mu"}, ExpectedStatusCode: http.StatusUnprocessableEntity},
				{SaveError: dashboards.ErrDashboardTypeMismatch, ExpectedStatusCode: http.StatusBadRequest},
				{SaveError: dashboards.ErrDashboardFolderWithSameNameAsDashboard, ExpectedStatusCode: http.StatusBadRequest},
				{SaveError: dashboards.ErrDashboardWithSameNameAsFolder, ExpectedStatusCode: http.StatusBadRequest},
				{SaveError: dashboards.ErrDashboardFolderNameExists, ExpectedStatusCode: http.StatusBadRequest},
				{SaveError: dashboards.ErrDashboardUpdateAccessDenied, ExpectedStatusCode: http.StatusForbidden},
				{SaveError: dashboards.ErrDashboardInvalidUid, ExpectedStatusCode: http.StatusBadRequest},
				{SaveError: dashboards.ErrDashboardUidTooLong, ExpectedStatusCode: http.StatusBadRequest},
				{SaveError: dashboards.ErrDashboardCannotSaveProvisionedDashboard, ExpectedStatusCode: http.StatusBadRequest},
				{SaveError: dashboards.UpdatePluginDashboardError{PluginId: "plug"}, ExpectedStatusCode: http.StatusPreconditionFailed},
			}

			cmd := dashboards.SaveDashboardCommand{
				OrgID: 1,
				Dashboard: simplejson.NewFromAny(map[string]any{
					"title": "",
				}),
			}

			for _, tc := range testCases {
				dashboardService := dashboards.NewFakeDashboardService(t)
				dashboardService.On("SaveDashboard", mock.Anything, mock.AnythingOfType("*dashboards.SaveDashboardDTO"), mock.AnythingOfType("bool")).Return(nil, tc.SaveError)

				postDashboardScenario(t, fmt.Sprintf("Expect '%s' error when calling POST on", tc.SaveError.Error()),
					"/api/dashboards", "/api/dashboards", cmd, dashboardService, nil, func(sc *scenarioContext) {
						callPostDashboard(sc)
						assert.Equal(t, tc.ExpectedStatusCode, sc.resp.Code, sc.resp.Body.String())
					})
			}
		})
	})

	t.Run("Given a dashboard to validate", func(t *testing.T) {
		sqlmock := dbtest.NewFakeDB()

		t.Run("When an invalid dashboard json is posted", func(t *testing.T) {
			cmd := dashboards.ValidateDashboardCommand{
				Dashboard: "{\"hello\": \"world\"}",
			}

			role := org.RoleAdmin
			postValidateScenario(t, "When calling POST on", "/api/dashboards/validate", "/api/dashboards/validate", cmd, role, func(sc *scenarioContext) {
				callPostDashboard(sc)

				result := sc.ToJSON()
				assert.Equal(t, http.StatusUnprocessableEntity, sc.resp.Code)
				assert.False(t, result.Get("isValid").MustBool())
				assert.NotEmpty(t, result.Get("message").MustString())
			}, sqlmock)
		})

		t.Run("When a dashboard with a too-low schema version is posted", func(t *testing.T) {
			cmd := dashboards.ValidateDashboardCommand{
				Dashboard: "{\"schemaVersion\": 1}",
			}

			role := org.RoleAdmin
			postValidateScenario(t, "When calling POST on", "/api/dashboards/validate", "/api/dashboards/validate", cmd, role, func(sc *scenarioContext) {
				callPostDashboard(sc)

				result := sc.ToJSON()
				assert.Equal(t, http.StatusPreconditionFailed, sc.resp.Code)
				assert.False(t, result.Get("isValid").MustBool())
				assert.Equal(t, "invalid schema version", result.Get("message").MustString())
			}, sqlmock)
		})

		t.Run("When a valid dashboard is posted", func(t *testing.T) {
			devenvDashboard, readErr := os.ReadFile("../../devenv/dev-dashboards/home.json")
			assert.Empty(t, readErr)

			cmd := dashboards.ValidateDashboardCommand{
				Dashboard: string(devenvDashboard),
			}

			role := org.RoleAdmin
			postValidateScenario(t, "When calling POST on", "/api/dashboards/validate", "/api/dashboards/validate", cmd, role, func(sc *scenarioContext) {
				callPostDashboard(sc)

				result := sc.ToJSON()
				assert.Equal(t, http.StatusOK, sc.resp.Code)
				assert.True(t, result.Get("isValid").MustBool())
			}, sqlmock)
		})
	})

	t.Run("Given two dashboards being compared", func(t *testing.T) {
		fakeDashboardVersionService := dashvertest.NewDashboardVersionServiceFake()
		fakeDashboardVersionService.ExpectedDashboardVersions = []*dashver.DashboardVersionDTO{
			{
				DashboardID: 1,
				Version:     1,
				Data: simplejson.NewFromAny(map[string]any{
					"title": "Dash1",
				}),
			},
			{
				DashboardID: 2,
				Version:     2,
				Data: simplejson.NewFromAny(map[string]any{
					"title": "Dash2",
				}),
			},
		}
		sqlmock := dbtest.NewFakeDB()
		cmd := dtos.CalculateDiffOptions{
			Base: dtos.CalculateDiffTarget{
				DashboardId: 1,
				Version:     1,
			},
			New: dtos.CalculateDiffTarget{
				DashboardId: 2,
				Version:     2,
			},
			DiffType: "basic",
		}

		t.Run("when user does not have permission", func(t *testing.T) {
			role := org.RoleViewer
			postDiffScenario(t, "When calling POST on", "/api/dashboards/calculate-diff", "/api/dashboards/calculate-diff", cmd, role, func(sc *scenarioContext) {
				guardian.MockDashboardGuardian(&guardian.FakeDashboardGuardian{CanSaveValue: false})

				callPostDashboard(sc)
				assert.Equal(t, http.StatusForbidden, sc.resp.Code)
			}, sqlmock, fakeDashboardVersionService)
		})

		t.Run("when user does have permission", func(t *testing.T) {
			role := org.RoleAdmin
			postDiffScenario(t, "When calling POST on", "/api/dashboards/calculate-diff", "/api/dashboards/calculate-diff", cmd, role, func(sc *scenarioContext) {
				guardian.MockDashboardGuardian(&guardian.FakeDashboardGuardian{CanSaveValue: true})
				// This test shouldn't hit GetDashboardACLInfoList, so no setup needed
				sc.dashboardVersionService = fakeDashboardVersionService
				callPostDashboard(sc)
				assert.Equal(t, http.StatusOK, sc.resp.Code)
			}, sqlmock, fakeDashboardVersionService)
		})
	})

	t.Run("Given dashboard in folder being restored should restore to folder", func(t *testing.T) {
		const folderID int64 = 1
		fakeDash := dashboards.NewDashboard("Child dash")
		fakeDash.ID = 2
		// nolint:staticcheck
		fakeDash.FolderID = folderID
		fakeDash.HasACL = false

		dashboardService := dashboards.NewFakeDashboardService(t)
		dashboardService.On("GetDashboard", mock.Anything, mock.AnythingOfType("*dashboards.GetDashboardQuery")).Return(fakeDash, nil)
		dashboardService.On("SaveDashboard", mock.Anything, mock.AnythingOfType("*dashboards.SaveDashboardDTO"), mock.AnythingOfType("bool")).Run(func(args mock.Arguments) {
			cmd := args.Get(1).(*dashboards.SaveDashboardDTO)
			cmd.Dashboard = &dashboards.Dashboard{
				ID: 2, UID: "uid", Title: "Dash", Slug: "dash", Version: 1,
			}
		}).Return(nil, nil)

		cmd := dtos.RestoreDashboardVersionCommand{
			Version: 1,
		}
		fakeDashboardVersionService := dashvertest.NewDashboardVersionServiceFake()
		fakeDashboardVersionService.ExpectedDashboardVersions = []*dashver.DashboardVersionDTO{
			{
				DashboardID: 2,
				Version:     1,
				Data:        fakeDash.Data,
			}}
		mockSQLStore := dbtest.NewFakeDB()
		origNewGuardian := guardian.New
		guardian.MockDashboardGuardian(&guardian.FakeDashboardGuardian{CanSaveValue: true})
		t.Cleanup(func() {
			guardian.New = origNewGuardian
		})

		restoreDashboardVersionScenario(t, "When calling POST on", "/api/dashboards/id/1/restore",
			"/api/dashboards/id/:dashboardId/restore", dashboardService, fakeDashboardVersionService, cmd, func(sc *scenarioContext) {
				sc.dashboardVersionService = fakeDashboardVersionService

				callRestoreDashboardVersion(sc)
				assert.Equal(t, http.StatusOK, sc.resp.Code)
			}, mockSQLStore)
	})

	t.Run("Given dashboard in general folder being restored should restore to general folder", func(t *testing.T) {
		fakeDash := dashboards.NewDashboard("Child dash")
		fakeDash.ID = 2
		fakeDash.HasACL = false

		dashboardService := dashboards.NewFakeDashboardService(t)
		dashboardService.On("GetDashboard", mock.Anything, mock.AnythingOfType("*dashboards.GetDashboardQuery")).Return(fakeDash, nil)
		dashboardService.On("SaveDashboard", mock.Anything, mock.AnythingOfType("*dashboards.SaveDashboardDTO"), mock.AnythingOfType("bool")).Run(func(args mock.Arguments) {
			cmd := args.Get(1).(*dashboards.SaveDashboardDTO)
			cmd.Dashboard = &dashboards.Dashboard{
				ID: 2, UID: "uid", Title: "Dash", Slug: "dash", Version: 1,
			}
		}).Return(nil, nil)

		fakeDashboardVersionService := dashvertest.NewDashboardVersionServiceFake()
		fakeDashboardVersionService.ExpectedDashboardVersions = []*dashver.DashboardVersionDTO{
			{
				DashboardID: 2,
				Version:     1,
				Data:        fakeDash.Data,
			}}

		cmd := dtos.RestoreDashboardVersionCommand{
			Version: 1,
		}
		mockSQLStore := dbtest.NewFakeDB()
		restoreDashboardVersionScenario(t, "When calling POST on", "/api/dashboards/id/1/restore",
			"/api/dashboards/id/:dashboardId/restore", dashboardService, fakeDashboardVersionService, cmd, func(sc *scenarioContext) {
				callRestoreDashboardVersion(sc)
				assert.Equal(t, http.StatusOK, sc.resp.Code)
			}, mockSQLStore)
	})

	t.Run("Given provisioned dashboard", func(t *testing.T) {
		mockSQLStore := dbtest.NewFakeDB()
		dashboardStore := dashboards.NewFakeDashboardStore(t)
		dashboardStore.On("GetProvisionedDataByDashboardID", mock.Anything, mock.AnythingOfType("int64")).Return(&dashboards.DashboardProvisioning{ExternalID: "/dashboard1.json"}, nil).Once()

		dashboardService := dashboards.NewFakeDashboardService(t)

		dataValue, err := simplejson.NewJson([]byte(`{"id": 1, "editable": true, "style": "dark"}`))
		require.NoError(t, err)
		qResult := &dashboards.Dashboard{ID: 1, Data: dataValue}
		dashboardService.On("GetDashboard", mock.Anything, mock.AnythingOfType("*dashboards.GetDashboardQuery")).Return(qResult, nil)
		guardian.MockDashboardGuardian(&guardian.FakeDashboardGuardian{CanViewValue: true})

		loggedInUserScenarioWithRole(t, "When calling GET on", "GET", "/api/dashboards/uid/dash", "/api/dashboards/uid/:uid", org.RoleEditor, func(sc *scenarioContext) {
			fakeProvisioningService := provisioning.NewProvisioningServiceMock(context.Background())
			fakeProvisioningService.GetDashboardProvisionerResolvedPathFunc = func(name string) string {
				return "/tmp/grafana/dashboards"
			}

			dash := getDashboardShouldReturn200WithConfig(t, sc, fakeProvisioningService, dashboardStore, dashboardService, nil)

			assert.Equal(t, "../../../dashboard1.json", dash.Meta.ProvisionedExternalId, mockSQLStore)
		}, mockSQLStore)

		loggedInUserScenarioWithRole(t, "When allowUiUpdates is true and calling GET on", "GET", "/api/dashboards/uid/dash", "/api/dashboards/uid/:uid", org.RoleEditor, func(sc *scenarioContext) {
			fakeProvisioningService := provisioning.NewProvisioningServiceMock(context.Background())
			fakeProvisioningService.GetDashboardProvisionerResolvedPathFunc = func(name string) string {
				return "/tmp/grafana/dashboards"
			}

			fakeProvisioningService.GetAllowUIUpdatesFromConfigFunc = func(name string) bool {
				return true
			}

			hs := &HTTPServer{
				Cfg:                          setting.NewCfg(),
				ProvisioningService:          fakeProvisioningService,
				LibraryPanelService:          &mockLibraryPanelService{},
				LibraryElementService:        &mockLibraryElementService{},
				dashboardProvisioningService: mockDashboardProvisioningService{},
				SQLStore:                     mockSQLStore,
				AccessControl:                accesscontrolmock.New(),
				DashboardService:             dashboardService,
				Features:                     featuremgmt.WithFeatures(),
				Kinds:                        corekind.NewBase(nil),
				starService:                  startest.NewStarServiceFake(),
			}
			hs.callGetDashboard(sc)

			assert.Equal(t, http.StatusOK, sc.resp.Code)

			dash := dtos.DashboardFullWithMeta{}
			err := json.NewDecoder(sc.resp.Body).Decode(&dash)
			require.NoError(t, err)

			assert.Equal(t, false, dash.Meta.Provisioned)
		}, mockSQLStore)
	})
}

func TestDashboardVersionsAPIEndpoint(t *testing.T) {
	fakeDash := dashboards.NewDashboard("Child dash")
	fakeDash.ID = 1
	// nolint:staticcheck
	fakeDash.FolderID = 1

	fakeDashboardVersionService := dashvertest.NewDashboardVersionServiceFake()
	dashboardService := dashboards.NewFakeDashboardService(t)
	dashboardService.On("GetDashboard", mock.Anything, mock.AnythingOfType("*dashboards.GetDashboardQuery")).Return(fakeDash, nil)

	mockSQLStore := dbtest.NewFakeDB()

	cfg := setting.NewCfg()

	getHS := func(userSvc *usertest.FakeUserService) *HTTPServer {
		return &HTTPServer{
			Cfg:                     cfg,
			pluginStore:             &pluginstore.FakePluginStore{},
			SQLStore:                mockSQLStore,
			AccessControl:           accesscontrolmock.New(),
			Features:                featuremgmt.WithFeatures(),
			DashboardService:        dashboardService,
			dashboardVersionService: fakeDashboardVersionService,
			Kinds:                   corekind.NewBase(nil),
			QuotaService:            quotatest.New(false, nil),
			userService:             userSvc,
			CacheService:            localcache.New(5*time.Minute, 10*time.Minute),
			log:                     log.New(),
		}
	}

	setUp := func() {
		guardian.MockDashboardGuardian(&guardian.FakeDashboardGuardian{CanSaveValue: true})
	}

	loggedInUserScenarioWithRole(t, "When user exists and calling GET on", "GET", "/api/dashboards/id/2/versions",
		"/api/dashboards/id/:dashboardId/versions", org.RoleEditor, func(sc *scenarioContext) {
			setUp()
			fakeDashboardVersionService.ExpectedListDashboarVersions = []*dashver.DashboardVersionDTO{
				{
					Version:   1,
					CreatedBy: 1,
				},
				{
					Version:   2,
					CreatedBy: 1,
				},
			}
			getHS(&usertest.FakeUserService{
				ExpectedUser: &user.User{ID: 1, Login: "test-user"},
			}).callGetDashboardVersions(sc)

			assert.Equal(t, http.StatusOK, sc.resp.Code)
			var versions []dashver.DashboardVersionMeta
			err := json.NewDecoder(sc.resp.Body).Decode(&versions)
			require.NoError(t, err)
			for _, v := range versions {
				assert.Equal(t, "test-user", v.CreatedBy)
			}
		}, mockSQLStore)

	loggedInUserScenarioWithRole(t, "When user does not exist and calling GET on", "GET", "/api/dashboards/id/2/versions",
		"/api/dashboards/id/:dashboardId/versions", org.RoleEditor, func(sc *scenarioContext) {
			setUp()
			fakeDashboardVersionService.ExpectedListDashboarVersions = []*dashver.DashboardVersionDTO{
				{
					Version:   1,
					CreatedBy: 1,
				},
				{
					Version:   2,
					CreatedBy: 1,
				},
			}
			getHS(&usertest.FakeUserService{
				ExpectedError: user.ErrUserNotFound,
			}).callGetDashboardVersions(sc)

			assert.Equal(t, http.StatusOK, sc.resp.Code)
			var versions []dashver.DashboardVersionMeta
			err := json.NewDecoder(sc.resp.Body).Decode(&versions)
			require.NoError(t, err)
			for _, v := range versions {
				assert.Equal(t, anonString, v.CreatedBy)
			}
		}, mockSQLStore)

	loggedInUserScenarioWithRole(t, "When failing to get user and calling GET on", "GET", "/api/dashboards/id/2/versions",
		"/api/dashboards/id/:dashboardId/versions", org.RoleEditor, func(sc *scenarioContext) {
			setUp()
			fakeDashboardVersionService.ExpectedListDashboarVersions = []*dashver.DashboardVersionDTO{
				{
					Version:   1,
					CreatedBy: 1,
				},
				{
					Version:   2,
					CreatedBy: 1,
				},
			}
			getHS(&usertest.FakeUserService{
				ExpectedError: fmt.Errorf("some error"),
			}).callGetDashboardVersions(sc)

			assert.Equal(t, http.StatusOK, sc.resp.Code)
			var versions []dashver.DashboardVersionMeta
			err := json.NewDecoder(sc.resp.Body).Decode(&versions)
			require.NoError(t, err)
			for _, v := range versions {
				assert.Equal(t, anonString, v.CreatedBy)
			}
		}, mockSQLStore)
}

func getDashboardShouldReturn200WithConfig(t *testing.T, sc *scenarioContext, provisioningService provisioning.ProvisioningService, dashboardStore dashboards.Store, dashboardService dashboards.DashboardService, folderStore folder.FolderStore) dtos.DashboardFullWithMeta {
	t.Helper()

	if provisioningService == nil {
		provisioningService = provisioning.NewProvisioningServiceMock(context.Background())
	}

	features := featuremgmt.WithFeatures()
	var err error
	if dashboardStore == nil {
		sql := db.InitTestDB(t)
		quotaService := quotatest.New(false, nil)
		dashboardStore, err = database.ProvideDashboardStore(sql, sql.Cfg, features, tagimpl.ProvideService(sql), quotaService)
		require.NoError(t, err)
	}

	libraryPanelsService := mockLibraryPanelService{}
	libraryElementsService := mockLibraryElementService{}
	cfg := setting.NewCfg()
	ac := accesscontrolmock.New()
	folderPermissions := accesscontrolmock.NewMockedPermissionsService()
	dashboardPermissions := accesscontrolmock.NewMockedPermissionsService()

	folderSvc := folderimpl.ProvideService(ac, bus.ProvideBus(tracing.InitializeTracerForTest()),
		cfg, dashboardStore, folderStore, db.InitTestDB(t), features)

	if dashboardService == nil {
		dashboardService, err = service.ProvideDashboardServiceImpl(
			cfg, dashboardStore, folderStore, nil, features, folderPermissions, dashboardPermissions,
			ac, folderSvc,
		)
		require.NoError(t, err)
	}

	dashboardProvisioningService, err := service.ProvideDashboardServiceImpl(
		cfg, dashboardStore, folderStore, nil, features, folderPermissions, dashboardPermissions,
		ac, folderSvc,
	)
	require.NoError(t, err)

	hs := &HTTPServer{
		Cfg:                          cfg,
		LibraryPanelService:          &libraryPanelsService,
		LibraryElementService:        &libraryElementsService,
		SQLStore:                     sc.sqlStore,
		ProvisioningService:          provisioningService,
		AccessControl:                accesscontrolmock.New(),
		dashboardProvisioningService: dashboardProvisioningService,
		DashboardService:             dashboardService,
		Features:                     featuremgmt.WithFeatures(),
		Kinds:                        corekind.NewBase(nil),
		starService:                  startest.NewStarServiceFake(),
	}

	hs.callGetDashboard(sc)

	require.Equal(sc.t, 200, sc.resp.Code)

	dash := dtos.DashboardFullWithMeta{}
	err = json.NewDecoder(sc.resp.Body).Decode(&dash)
	require.NoError(sc.t, err)

	return dash
}

func (hs *HTTPServer) callGetDashboard(sc *scenarioContext) {
	sc.handlerFunc = hs.GetDashboard
	sc.fakeReqWithParams("GET", sc.url, map[string]string{}).exec()
}

func (hs *HTTPServer) callGetDashboardVersions(sc *scenarioContext) {
	sc.handlerFunc = hs.GetDashboardVersions
	sc.fakeReqWithParams("GET", sc.url, map[string]string{}).exec()
}

func callPostDashboard(sc *scenarioContext) {
	sc.fakeReqWithParams("POST", sc.url, map[string]string{}).exec()
}

func callRestoreDashboardVersion(sc *scenarioContext) {
	sc.fakeReqWithParams("POST", sc.url, map[string]string{}).exec()
}

func callPostDashboardShouldReturnSuccess(sc *scenarioContext) {
	callPostDashboard(sc)

	assert.Equal(sc.t, 200, sc.resp.Code)
}

func postDashboardScenario(t *testing.T, desc string, url string, routePattern string, cmd dashboards.SaveDashboardCommand, dashboardService dashboards.DashboardService, folderService folder.Service, fn scenarioFunc) {
	t.Run(fmt.Sprintf("%s %s", desc, url), func(t *testing.T) {
		cfg := setting.NewCfg()
		hs := HTTPServer{
			Cfg:                   cfg,
			ProvisioningService:   provisioning.NewProvisioningServiceMock(context.Background()),
			Live:                  newTestLive(t, db.InitTestDB(t)),
			QuotaService:          quotatest.New(false, nil),
			pluginStore:           &pluginstore.FakePluginStore{},
			LibraryPanelService:   &mockLibraryPanelService{},
			LibraryElementService: &mockLibraryElementService{},
			DashboardService:      dashboardService,
			folderService:         folderService,
			Features:              featuremgmt.WithFeatures(),
			Kinds:                 corekind.NewBase(nil),
			accesscontrolService:  actest.FakeService{},
			log:                   log.New("test-logger"),
		}

		sc := setupScenarioContext(t, url)
		sc.defaultHandler = routing.Wrap(func(c *contextmodel.ReqContext) response.Response {
			c.Req.Body = mockRequestBody(cmd)
			c.Req.Header.Add("Content-Type", "application/json")
			sc.context = c
			sc.context.SignedInUser = &user.SignedInUser{OrgID: cmd.OrgID, UserID: cmd.UserID}

			return hs.PostDashboard(c)
		})

		sc.m.Post(routePattern, sc.defaultHandler)

		fn(sc)
	})
}

func postValidateScenario(t *testing.T, desc string, url string, routePattern string, cmd dashboards.ValidateDashboardCommand,
	role org.RoleType, fn scenarioFunc, sqlmock db.DB) {
	t.Run(fmt.Sprintf("%s %s", desc, url), func(t *testing.T) {
		cfg := setting.NewCfg()
		hs := HTTPServer{
			Cfg:                   cfg,
			ProvisioningService:   provisioning.NewProvisioningServiceMock(context.Background()),
			Live:                  newTestLive(t, db.InitTestDB(t)),
			QuotaService:          quotatest.New(false, nil),
			LibraryPanelService:   &mockLibraryPanelService{},
			LibraryElementService: &mockLibraryElementService{},
			SQLStore:              sqlmock,
			Features:              featuremgmt.WithFeatures(),
			Kinds:                 corekind.NewBase(nil),
		}

		sc := setupScenarioContext(t, url)
		sc.defaultHandler = routing.Wrap(func(c *contextmodel.ReqContext) response.Response {
			c.Req.Body = mockRequestBody(cmd)
			c.Req.Header.Add("Content-Type", "application/json")
			sc.context = c
			sc.context.SignedInUser = &user.SignedInUser{
				OrgID:  testOrgID,
				UserID: testUserID,
			}
			sc.context.OrgRole = role

			return hs.ValidateDashboard(c)
		})

		sc.m.Post(routePattern, sc.defaultHandler)

		fn(sc)
	})
}

func postDiffScenario(t *testing.T, desc string, url string, routePattern string, cmd dtos.CalculateDiffOptions,
	role org.RoleType, fn scenarioFunc, sqlmock db.DB, fakeDashboardVersionService *dashvertest.FakeDashboardVersionService) {
	t.Run(fmt.Sprintf("%s %s", desc, url), func(t *testing.T) {
		cfg := setting.NewCfg()

		dashSvc := dashboards.NewFakeDashboardService(t)
		hs := HTTPServer{
			Cfg:                     cfg,
			ProvisioningService:     provisioning.NewProvisioningServiceMock(context.Background()),
			Live:                    newTestLive(t, db.InitTestDB(t)),
			QuotaService:            quotatest.New(false, nil),
			LibraryPanelService:     &mockLibraryPanelService{},
			LibraryElementService:   &mockLibraryElementService{},
			SQLStore:                sqlmock,
			dashboardVersionService: fakeDashboardVersionService,
			Features:                featuremgmt.WithFeatures(),
			Kinds:                   corekind.NewBase(nil),
			DashboardService:        dashSvc,
		}

		sc := setupScenarioContext(t, url)
		sc.defaultHandler = routing.Wrap(func(c *contextmodel.ReqContext) response.Response {
			c.Req.Body = mockRequestBody(cmd)
			c.Req.Header.Add("Content-Type", "application/json")
			sc.context = c
			sc.context.SignedInUser = &user.SignedInUser{
				OrgID:  testOrgID,
				UserID: testUserID,
			}
			sc.context.OrgRole = role

			return hs.CalculateDashboardDiff(c)
		})

		sc.m.Post(routePattern, sc.defaultHandler)

		fn(sc)
	})
}

func restoreDashboardVersionScenario(t *testing.T, desc string, url string, routePattern string,
	mock *dashboards.FakeDashboardService, fakeDashboardVersionService *dashvertest.FakeDashboardVersionService,
	cmd dtos.RestoreDashboardVersionCommand, fn scenarioFunc, sqlStore db.DB) {
	t.Run(fmt.Sprintf("%s %s", desc, url), func(t *testing.T) {
		cfg := setting.NewCfg()
		folderSvc := foldertest.NewFakeService()
		folderSvc.ExpectedFolder = &folder.Folder{}

		hs := HTTPServer{
			Cfg:                     cfg,
			ProvisioningService:     provisioning.NewProvisioningServiceMock(context.Background()),
			Live:                    newTestLive(t, db.InitTestDB(t)),
			QuotaService:            quotatest.New(false, nil),
			LibraryPanelService:     &mockLibraryPanelService{},
			LibraryElementService:   &mockLibraryElementService{},
			DashboardService:        mock,
			SQLStore:                sqlStore,
			Features:                featuremgmt.WithFeatures(),
			dashboardVersionService: fakeDashboardVersionService,
			Kinds:                   corekind.NewBase(nil),
			accesscontrolService:    actest.FakeService{},
			folderService:           folderSvc,
		}

		sc := setupScenarioContext(t, url)
		sc.sqlStore = sqlStore
		sc.dashboardVersionService = fakeDashboardVersionService
		sc.defaultHandler = routing.Wrap(func(c *contextmodel.ReqContext) response.Response {
			c.Req.Body = mockRequestBody(cmd)
			c.Req.Header.Add("Content-Type", "application/json")
			sc.context = c
			sc.context.SignedInUser = &user.SignedInUser{
				OrgID:  testOrgID,
				UserID: testUserID,
			}
			sc.context.OrgRole = org.RoleAdmin

			return hs.RestoreDashboardVersion(c)
		})

		sc.m.Post(routePattern, sc.defaultHandler)

		fn(sc)
	})
}

func (sc *scenarioContext) ToJSON() *simplejson.Json {
	result := simplejson.New()
	err := json.NewDecoder(sc.resp.Body).Decode(result)
	require.NoError(sc.t, err)
	return result
}

type mockDashboardProvisioningService struct {
	dashboards.DashboardProvisioningService
}

func (s mockDashboardProvisioningService) GetProvisionedDashboardDataByDashboardID(ctx context.Context, dashboardID int64) (
	*dashboards.DashboardProvisioning, error) {
	return nil, nil
}

type mockLibraryPanelService struct {
}

var _ librarypanels.Service = (*mockLibraryPanelService)(nil)

func (m *mockLibraryPanelService) ConnectLibraryPanelsForDashboard(c context.Context, signedInUser identity.Requester, dash *dashboards.Dashboard) error {
	return nil
}

func (m *mockLibraryPanelService) ImportLibraryPanelsForDashboard(c context.Context, signedInUser identity.Requester, libraryPanels *simplejson.Json, panels []any, folderID int64) error {
	return nil
}

type mockLibraryElementService struct {
}

func (l *mockLibraryElementService) CreateElement(c context.Context, signedInUser identity.Requester, cmd model.CreateLibraryElementCommand) (model.LibraryElementDTO, error) {
	return model.LibraryElementDTO{}, nil
}

// GetElement gets an element from a UID.
func (l *mockLibraryElementService) GetElement(c context.Context, signedInUser identity.Requester, cmd model.GetLibraryElementCommand) (model.LibraryElementDTO, error) {
	return model.LibraryElementDTO{}, nil
}

// GetElementsForDashboard gets all connected elements for a specific dashboard.
func (l *mockLibraryElementService) GetElementsForDashboard(c context.Context, dashboardID int64) (map[string]model.LibraryElementDTO, error) {
	return map[string]model.LibraryElementDTO{}, nil
}

// ConnectElementsToDashboard connects elements to a specific dashboard.
func (l *mockLibraryElementService) ConnectElementsToDashboard(c context.Context, signedInUser identity.Requester, elementUIDs []string, dashboardID int64) error {
	return nil
}

// DisconnectElementsFromDashboard disconnects elements from a specific dashboard.
func (l *mockLibraryElementService) DisconnectElementsFromDashboard(c context.Context, dashboardID int64) error {
	return nil
}

// DeleteLibraryElementsInFolder deletes all elements for a specific folder.
func (l *mockLibraryElementService) DeleteLibraryElementsInFolder(c context.Context, signedInUser identity.Requester, folderUID string) error {
	return nil
}
