// Copyright 2016 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/tsuru/config"
	"github.com/tsuru/tsuru/app"
	"github.com/tsuru/tsuru/app/image"
	"github.com/tsuru/tsuru/auth"
	"github.com/tsuru/tsuru/db"
	"github.com/tsuru/tsuru/db/dbtest"
	"github.com/tsuru/tsuru/event"
	"github.com/tsuru/tsuru/event/eventtest"
	"github.com/tsuru/tsuru/permission"
	"github.com/tsuru/tsuru/provision"
	"github.com/tsuru/tsuru/provision/provisiontest"
	"github.com/tsuru/tsuru/repository"
	"github.com/tsuru/tsuru/repository/repositorytest"
	"github.com/tsuru/tsuru/router/routertest"
	"gopkg.in/check.v1"
	"gopkg.in/mgo.v2/bson"
)

type DeploySuite struct {
	conn        *db.Storage
	logConn     *db.LogStorage
	token       auth.Token
	team        *auth.Team
	provisioner *provisiontest.FakeProvisioner
}

var _ = check.Suite(&DeploySuite{})

func (s *DeploySuite) createUserAndTeam(c *check.C) {
	user := &auth.User{Email: "whydidifall@thewho.com", Password: "123456"}
	app.AuthScheme = nativeScheme
	_, err := nativeScheme.Create(user)
	c.Assert(err, check.IsNil)
	s.team = &auth.Team{Name: "tsuruteam"}
	err = s.conn.Teams().Insert(s.team)
	c.Assert(err, check.IsNil)
	s.token = userWithPermission(c, permission.Permission{
		Scheme:  permission.PermAppReadDeploy,
		Context: permission.Context(permission.CtxTeam, s.team.Name),
	}, permission.Permission{
		Scheme:  permission.PermAppDeploy,
		Context: permission.Context(permission.CtxTeam, s.team.Name),
	})
}

func (s *DeploySuite) SetUpSuite(c *check.C) {
	err := config.ReadConfigFile("testdata/config.yaml")
	c.Assert(err, check.IsNil)
	config.Set("database:url", "127.0.0.1:27017")
	config.Set("database:name", "tsuru_deploy_api_tests")
	config.Set("auth:hash-cost", 4)
	config.Set("repo-manager", "fake")
	s.conn, err = db.Conn()
	c.Assert(err, check.IsNil)
	s.logConn, err = db.LogConn()
	c.Assert(err, check.IsNil)
}

func (s *DeploySuite) TearDownSuite(c *check.C) {
	config.Unset("docker:router")
	provision.RemovePool("pool1")
	s.conn.Apps().Database.DropDatabase()
	s.logConn.Logs("myapp").Database.DropDatabase()
	s.conn.Close()
	s.logConn.Close()
}

func (s *DeploySuite) SetUpTest(c *check.C) {
	s.provisioner = provisiontest.ProvisionerInstance
	provision.DefaultProvisioner = "fake"
	s.provisioner.Reset()
	routertest.FakeRouter.Reset()
	repositorytest.Reset()
	err := dbtest.ClearAllCollections(s.conn.Apps().Database)
	c.Assert(err, check.IsNil)
	s.createUserAndTeam(c)
	s.conn.Platforms().Insert(app.Platform{Name: "python"})
	opts := provision.AddPoolOptions{Name: "pool1", Default: true}
	err = provision.AddPool(opts)
	c.Assert(err, check.IsNil)
	user, err := s.token.User()
	c.Assert(err, check.IsNil)
	repository.Manager().CreateUser(user.Email)
	config.Set("docker:router", "fake")
}

func (s *DeploySuite) TestDeployHandler(c *check.C) {
	a := app.App{Name: "otherapp", Platform: "python", TeamOwner: s.team.Name}
	user, _ := s.token.User()
	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	url := fmt.Sprintf("/apps/%s/repository/clone", a.Name)
	request, err := http.NewRequest("POST", url, strings.NewReader("archive-url=http://something.tar.gz&user=fulano"))
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Header().Get("Content-Type"), check.Equals, "text")
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Body.String(), check.Equals, "Archive deploy called\nOK\n")
	c.Assert(eventtest.EventDesc{
		Target: appTarget(a.Name),
		Owner:  s.token.GetUserName(),
		Kind:   "app.deploy",
		StartCustomData: map[string]interface{}{
			"app.name":   a.Name,
			"commit":     "",
			"filesize":   0,
			"kind":       "archive-url",
			"archiveurl": "http://something.tar.gz",
			"user":       s.token.GetUserName(),
			"image":      "",
			"origin":     "",
			"build":      false,
			"rollback":   false,
		},
		EndCustomData: map[string]interface{}{
			"image": "app-image",
		},
		LogMatches: `Archive deploy called`,
	}, eventtest.HasEvent)
}

func (s *DeploySuite) TestDeployOriginDragAndDrop(c *check.C) {
	user, _ := s.token.User()
	a := app.App{Name: "otherapp", Platform: "python", TeamOwner: s.team.Name}
	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	url := fmt.Sprintf("/apps/%s/repository/clone?origin=drag-and-drop", a.Name)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", "archive.tar.gz")
	c.Assert(err, check.IsNil)
	file.Write([]byte("hello world!"))
	writer.Close()
	request, err := http.NewRequest("POST", url, &body)
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "multipart/form-data; boundary="+writer.Boundary())
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	recorder := httptest.NewRecorder()
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Header().Get("Content-Type"), check.Equals, "text")
	c.Assert(recorder.Body.String(), check.Equals, "Upload deploy called\nOK\n")
	c.Assert(eventtest.EventDesc{
		Target: appTarget(a.Name),
		Owner:  s.token.GetUserName(),
		Kind:   "app.deploy",
		StartCustomData: map[string]interface{}{
			"app.name":   a.Name,
			"commit":     "",
			"filesize":   12,
			"kind":       "upload",
			"archiveurl": "",
			"user":       s.token.GetUserName(),
			"image":      "",
			"origin":     "drag-and-drop",
			"build":      false,
			"rollback":   false,
		},
		EndCustomData: map[string]interface{}{
			"image": "app-image",
		},
		LogMatches: `Upload deploy called`,
	}, eventtest.HasEvent)
}

func (s *DeploySuite) TestDeployInvalidOrigin(c *check.C) {
	a := app.App{Name: "otherapp", Platform: "python", TeamOwner: s.team.Name}
	user, _ := s.token.User()
	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	url := fmt.Sprintf("/apps/%s/repository/clone?:appname=%s&origin=drag", a.Name, a.Name)
	request, err := http.NewRequest("POST", url, strings.NewReader("archive-url=http://something.tar.gz&user=fulano"))
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusBadRequest)
	c.Assert(recorder.Body.String(), check.Equals, "Invalid deployment origin\n")
}

func (s *DeploySuite) TestDeployOriginImage(c *check.C) {
	user, _ := s.token.User()
	a := app.App{Name: "otherapp", Platform: "python", TeamOwner: s.team.Name}
	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	url := fmt.Sprintf("/apps/%s/deploy?origin=app-deploy", a.Name)
	request, err := http.NewRequest("POST", url, strings.NewReader("image=127.0.0.1:5000/tsuru/otherapp"))
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Body.String(), check.Equals, "Image deploy called\nOK\n")
	c.Assert(eventtest.EventDesc{
		Target: appTarget(a.Name),
		Owner:  s.token.GetUserName(),
		Kind:   "app.deploy",
		StartCustomData: map[string]interface{}{
			"app.name":   a.Name,
			"commit":     "",
			"filesize":   0,
			"kind":       "image",
			"archiveurl": "",
			"user":       s.token.GetUserName(),
			"image":      "127.0.0.1:5000/tsuru/otherapp",
			"origin":     "image",
			"build":      false,
			"rollback":   false,
		},
		EndCustomData: map[string]interface{}{
			"image": "127.0.0.1:5000/tsuru/otherapp",
		},
		LogMatches: `Image deploy called`,
	}, eventtest.HasEvent)
}

func (s *DeploySuite) TestDeployArchiveURL(c *check.C) {
	user, _ := s.token.User()
	a := app.App{
		Name:      "otherapp",
		Plan:      app.Plan{Router: "fake"},
		Platform:  "python",
		TeamOwner: s.team.Name,
	}
	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	url := fmt.Sprintf("/apps/%s/repository/clone?:appname=%s", a.Name, a.Name)
	request, err := http.NewRequest("POST", url, strings.NewReader("archive-url=http://something.tar.gz&user=fulano"))
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Header().Get("Content-Type"), check.Equals, "text")
	c.Assert(recorder.Body.String(), check.Equals, "Archive deploy called\nOK\n")
	c.Assert(eventtest.EventDesc{
		Target: appTarget(a.Name),
		Owner:  s.token.GetUserName(),
		Kind:   "app.deploy",
		StartCustomData: map[string]interface{}{
			"app.name":   a.Name,
			"commit":     "",
			"filesize":   0,
			"kind":       "archive-url",
			"archiveurl": "http://something.tar.gz",
			"user":       s.token.GetUserName(),
			"image":      "",
			"origin":     "",
			"build":      false,
			"rollback":   false,
		},
		EndCustomData: map[string]interface{}{
			"image": "app-image",
		},
		LogMatches: `Archive deploy called`,
	}, eventtest.HasEvent)
}

func (s *DeploySuite) TestDeployUploadFile(c *check.C) {
	user, _ := s.token.User()
	a := app.App{
		Name:      "otherapp",
		Platform:  "python",
		Plan:      app.Plan{Router: "fake"},
		TeamOwner: s.team.Name,
	}

	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	url := fmt.Sprintf("/apps/%s/repository/clone", a.Name)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", "archive.tar.gz")
	c.Assert(err, check.IsNil)
	file.Write([]byte("hello world!"))
	writer.Close()
	request, err := http.NewRequest("POST", url, &body)
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "multipart/form-data; boundary="+writer.Boundary())
	recorder := httptest.NewRecorder()
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Header().Get("Content-Type"), check.Equals, "text")
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Body.String(), check.Equals, "Upload deploy called\nOK\n")
	c.Assert(eventtest.EventDesc{
		Target: appTarget(a.Name),
		Owner:  s.token.GetUserName(),
		Kind:   "app.deploy",
		StartCustomData: map[string]interface{}{
			"app.name":   a.Name,
			"commit":     "",
			"filesize":   12,
			"kind":       "upload",
			"archiveurl": "",
			"user":       s.token.GetUserName(),
			"image":      "",
			"origin":     "",
			"build":      false,
			"rollback":   false,
		},
		EndCustomData: map[string]interface{}{
			"image": "app-image",
		},
		LogMatches: `Upload deploy called`,
	}, eventtest.HasEvent)
}

func (s *DeploySuite) TestDeployWithCommit(c *check.C) {
	token, err := nativeScheme.AppLogin(app.InternalAppName)
	c.Assert(err, check.IsNil)
	user, _ := s.token.User()
	a := app.App{
		Name:      "otherapp",
		Platform:  "python",
		TeamOwner: s.team.Name,
		Plan:      app.Plan{Router: "fake"},
	}
	err = app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	url := fmt.Sprintf("/apps/%s/repository/clone", a.Name)
	request, err := http.NewRequest("POST", url, strings.NewReader("archive-url=http://something.tar.gz&user=fulano&commit=123"))
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	request.Header.Set("Authorization", "bearer "+token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Header().Get("Content-Type"), check.Equals, "text")
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Body.String(), check.Equals, "Archive deploy called\nOK\n")
	c.Assert(eventtest.EventDesc{
		Target: appTarget(a.Name),
		Owner:  "fulano",
		Kind:   "app.deploy",
		StartCustomData: map[string]interface{}{
			"app.name":   a.Name,
			"commit":     "123",
			"filesize":   0,
			"kind":       "git",
			"archiveurl": "http://something.tar.gz",
			"user":       "fulano",
			"image":      "",
			"origin":     "git",
			"build":      false,
			"rollback":   false,
			"message":    "msg1",
		},
		EndCustomData: map[string]interface{}{
			"image": "app-image",
		},
		LogMatches: `Archive deploy called`,
	}, eventtest.HasEvent)
}

func (s *DeploySuite) TestDeployWithCommitUserToken(c *check.C) {
	user, _ := s.token.User()
	a := app.App{
		Name:      "otherapp",
		Platform:  "python",
		TeamOwner: s.team.Name,
		Plan:      app.Plan{Router: "fake"},
	}
	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	url := fmt.Sprintf("/apps/%s/repository/clone", a.Name)
	request, err := http.NewRequest("POST", url, strings.NewReader("archive-url=http://something.tar.gz&user=fulano&commit=123"))
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Header().Get("Content-Type"), check.Equals, "text")
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Body.String(), check.Equals, "Archive deploy called\nOK\n")
	c.Assert(eventtest.EventDesc{
		Target: appTarget(a.Name),
		Owner:  s.token.GetUserName(),
		Kind:   "app.deploy",
		StartCustomData: map[string]interface{}{
			"app.name":   a.Name,
			"commit":     "",
			"filesize":   0,
			"kind":       "archive-url",
			"archiveurl": "http://something.tar.gz",
			"user":       s.token.GetUserName(),
			"image":      "",
			"origin":     "",
			"build":      false,
			"rollback":   false,
		},
		EndCustomData: map[string]interface{}{
			"image": "app-image",
		},
		LogMatches: `Archive deploy called`,
	}, eventtest.HasEvent)
}

func (s *DeploySuite) TestDeployWithMessage(c *check.C) {
	token, err := nativeScheme.AppLogin(app.InternalAppName)
	c.Assert(err, check.IsNil)
	user, _ := s.token.User()
	a := app.App{
		Name:      "otherapp",
		Platform:  "python",
		TeamOwner: s.team.Name,
		Plan:      app.Plan{Router: "fake"},
	}
	err = app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	url := fmt.Sprintf("/apps/%s/repository/clone", a.Name)
	request, err := http.NewRequest("POST", url, strings.NewReader("archive-url=http://something.tar.gz&message=and when he falleth"))
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	request.Header.Set("Authorization", "bearer "+token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Header().Get("Content-Type"), check.Equals, "text")
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Body.String(), check.Equals, "Archive deploy called\nOK\n")
	c.Assert(eventtest.EventDesc{
		Target: appTarget(a.Name),
		Owner:  token.GetUserName(),
		Kind:   "app.deploy",
		StartCustomData: map[string]interface{}{
			"app.name":   a.Name,
			"commit":     "",
			"filesize":   0,
			"kind":       "archive-url",
			"archiveurl": "http://something.tar.gz",
			"user":       token.GetUserName(),
			"image":      "",
			"origin":     "",
			"build":      false,
			"rollback":   false,
			"message":    "and when he falleth",
		},
		EndCustomData: map[string]interface{}{
			"image": "app-image",
		},
		LogMatches: `Archive deploy called`,
	}, eventtest.HasEvent)
}

func (s *DeploySuite) TestDeployDockerImage(c *check.C) {
	user, _ := s.token.User()
	a := app.App{Name: "otherapp", Platform: "python", TeamOwner: s.team.Name}
	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	url := fmt.Sprintf("/apps/%s/deploy", a.Name)
	request, err := http.NewRequest("POST", url, strings.NewReader("image=127.0.0.1:5000/tsuru/otherapp"))
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Body.String(), check.Equals, "Image deploy called\nOK\n")
	c.Assert(eventtest.EventDesc{
		Target: appTarget(a.Name),
		Owner:  s.token.GetUserName(),
		Kind:   "app.deploy",
		StartCustomData: map[string]interface{}{
			"app.name":   a.Name,
			"commit":     "",
			"filesize":   0,
			"kind":       "image",
			"archiveurl": "",
			"user":       s.token.GetUserName(),
			"image":      "127.0.0.1:5000/tsuru/otherapp",
			"origin":     "image",
			"build":      false,
			"rollback":   false,
		},
		EndCustomData: map[string]interface{}{
			"image": "127.0.0.1:5000/tsuru/otherapp",
		},
		LogMatches: `Image deploy called`,
	}, eventtest.HasEvent)
}

func (s *DeploySuite) TestDeployShouldIncrementDeployNumberOnApp(c *check.C) {
	user, _ := s.token.User()
	a := app.App{Name: "otherapp", Platform: "python", TeamOwner: s.team.Name}
	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	url := fmt.Sprintf("/apps/%s/repository/clone?:appname=%s", a.Name, a.Name)
	request, err := http.NewRequest("POST", url, strings.NewReader("archive-url=http://something.tar.gz"))
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	s.conn.Apps().Find(bson.M{"name": a.Name}).One(&a)
	c.Assert(a.Deploys, check.Equals, uint(1))
}

func (s *DeploySuite) TestDeployShouldReturnNotFoundWhenAppDoesNotExist(c *check.C) {
	request, err := http.NewRequest("POST", "/apps/abc/repository/clone", strings.NewReader("archive-url=http://something.tar.gz"))
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusNotFound)
	message := recorder.Body.String()
	c.Assert(message, check.Equals, "App not found.\n")
}

func (s *DeploySuite) TestDeployShouldReturnForbiddenWhenUserDoesNotHaveAccessToApp(c *check.C) {
	user := &auth.User{Email: "someone@tsuru.io", Password: "123456"}
	_, err := nativeScheme.Create(user)
	c.Assert(err, check.IsNil)
	token, err := nativeScheme.Login(map[string]string{"email": user.Email, "password": "123456"})
	c.Assert(err, check.IsNil)
	adminUser, _ := s.token.User()
	a := app.App{Name: "otherapp", Platform: "python", TeamOwner: s.team.Name}
	err = app.CreateApp(&a, adminUser)
	c.Assert(err, check.IsNil)
	url := fmt.Sprintf("/apps/%s/repository/clone?:appname=%s", a.Name, a.Name)
	request, err := http.NewRequest("POST", url, strings.NewReader("archive-url=http://something.tar.gz&user=fulano"))
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	request.Header.Set("Authorization", "bearer "+token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusForbidden)
	c.Assert(recorder.Body.String(), check.Equals, "User does not have permission to do this action in this app\n")
}

func (s *DeploySuite) TestDeployShouldReturnForbiddenWhenTokenIsntFromTheApp(c *check.C) {
	user, _ := s.token.User()
	app1 := app.App{Name: "otherapp", Platform: "python", TeamOwner: s.team.Name}
	err := app.CreateApp(&app1, user)
	c.Assert(err, check.IsNil)
	app2 := app.App{Name: "superapp", Platform: "python", TeamOwner: s.team.Name}
	err = app.CreateApp(&app2, user)
	c.Assert(err, check.IsNil)
	token, err := nativeScheme.AppLogin(app2.Name)
	c.Assert(err, check.IsNil)
	url := fmt.Sprintf("/apps/%s/repository/clone?:appname=%s", app1.Name, app2.Name)
	request, err := http.NewRequest("POST", url, strings.NewReader("archive-url=http://something.tar.gz&user=fulano"))
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	request.Header.Set("Authorization", "bearer "+token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusUnauthorized)
	c.Assert(recorder.Body.String(), check.Equals, "invalid app token\n")
}

func (s *DeploySuite) TestDeployWithTokenForInternalAppName(c *check.C) {
	token, err := nativeScheme.AppLogin(app.InternalAppName)
	c.Assert(err, check.IsNil)
	a := app.App{
		Name:      "otherapp",
		Platform:  "python",
		TeamOwner: s.team.Name,
		Plan:      app.Plan{Router: "fake"},
	}
	user, _ := s.token.User()
	err = app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	url := fmt.Sprintf("/apps/%s/repository/clone?:appname=%s", a.Name, a.Name)
	request, err := http.NewRequest("POST", url, strings.NewReader("archive-url=http://something.tar.gz&user=fulano"))
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	request.Header.Set("Authorization", "bearer "+token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Header().Get("Content-Type"), check.Equals, "text")
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Body.String(), check.Equals, "Archive deploy called\nOK\n")
}

func (s *DeploySuite) TestDeployWithoutArchiveURL(c *check.C) {
	user, _ := s.token.User()
	a := app.App{Name: "abc", Platform: "python", TeamOwner: s.team.Name}
	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	request, err := http.NewRequest("POST", "/apps/abc/repository/clone", nil)
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusBadRequest)
	message := recorder.Body.String()
	c.Assert(message, check.Equals, "you must specify either the archive-url, a image url or upload a file.\n")
}

func (s *DeploySuite) TestPermSchemeForDeploy(c *check.C) {
	var tests = []struct {
		input    app.DeployOptions
		expected *permission.PermissionScheme
	}{
		{
			app.DeployOptions{Commit: "abc123"},
			permission.PermAppDeployGit,
		},
		{
			app.DeployOptions{Image: "quay.io/tsuru/python"},
			permission.PermAppDeployImage,
		},
		{
			app.DeployOptions{File: ioutil.NopCloser(bytes.NewReader(nil))},
			permission.PermAppDeployUpload,
		},
		{
			app.DeployOptions{File: ioutil.NopCloser(bytes.NewReader(nil)), Build: true},
			permission.PermAppDeployBuild,
		},
		{
			app.DeployOptions{},
			permission.PermAppDeployArchiveUrl,
		},
	}
	for _, t := range tests {
		c.Check(permSchemeForDeploy(t.input), check.Equals, t.expected)
	}
}

func insertDeploysAsEvents(data []app.DeployData, c *check.C) []*event.Event {
	evts := make([]*event.Event, len(data))
	for i, d := range data {
		evt, err := event.New(&event.Opts{
			Target:   event.Target{Type: "app", Value: d.App},
			Kind:     permission.PermAppDeploy,
			RawOwner: event.Owner{Type: event.OwnerTypeUser, Name: d.User},
			CustomData: app.DeployOptions{
				Commit: d.Commit,
				Origin: d.Origin,
			},
			Allowed: event.Allowed(permission.PermAppReadEvents, permission.Context(permission.CtxApp, d.App)),
		})
		c.Assert(err, check.IsNil)
		evt.StartTime = d.Timestamp
		evt.Logf(d.Log)
		err = evt.SetOtherCustomData(map[string]string{"diff": d.Diff})
		c.Assert(err, check.IsNil)
		err = evt.DoneCustomData(nil, map[string]string{"image": d.Image})
		c.Assert(err, check.IsNil)
		evts[i] = evt
	}
	return evts
}

func (s *DeploySuite) TestDeployListNonAdmin(c *check.C) {
	user := &auth.User{Email: "nonadmin@nonadmin.com", Password: "123456"}
	app.AuthScheme = nativeScheme
	_, err := nativeScheme.Create(user)
	c.Assert(err, check.IsNil)
	team := &auth.Team{Name: "newteam"}
	err = s.conn.Teams().Insert(team)
	c.Assert(err, check.IsNil)
	token := customUserWithPermission(c, "apponlyg1", permission.Permission{
		Scheme:  permission.PermAppReadDeploy,
		Context: permission.Context(permission.CtxApp, "g1"),
	})
	a := app.App{Name: "g1", Platform: "python", TeamOwner: team.Name}
	err = app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	var result []app.DeployData
	request, err := http.NewRequest("GET", "/deploys", nil)
	c.Assert(err, check.IsNil)
	recorder := httptest.NewRecorder()
	timestamp := time.Date(2013, time.November, 1, 0, 0, 0, 0, time.Local)
	insertDeploysAsEvents([]app.DeployData{
		{App: "g1", Timestamp: timestamp.Add(time.Minute)},
		{App: "ge", Timestamp: timestamp.Add(time.Second)},
	}, c)
	request.Header.Set("Authorization", "bearer "+token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Header().Get("Content-Type"), check.Equals, "application/json")
	err = json.Unmarshal(recorder.Body.Bytes(), &result)
	c.Assert(err, check.IsNil)
	c.Assert(result, check.HasLen, 1)
	c.Assert(result[0].ID, check.NotNil)
	c.Assert(result[0].App, check.Equals, "g1")
	c.Assert(result[0].Timestamp.In(time.UTC), check.DeepEquals, timestamp.Add(time.Minute).In(time.UTC))
}

func (s *DeploySuite) TestDeployList(c *check.C) {
	user, _ := s.token.User()
	app1 := app.App{Name: "g1", Platform: "python", TeamOwner: s.team.Name}
	err := app.CreateApp(&app1, user)
	c.Assert(err, check.IsNil)
	app2 := app.App{Name: "ge", Platform: "python", TeamOwner: s.team.Name}
	err = app.CreateApp(&app2, user)
	c.Assert(err, check.IsNil)
	var result []app.DeployData
	request, err := http.NewRequest("GET", "/deploys", nil)
	c.Assert(err, check.IsNil)
	recorder := httptest.NewRecorder()
	timestamp := time.Date(2013, time.November, 1, 0, 0, 0, 0, time.Local)
	deps := []app.DeployData{
		{App: "g1", Timestamp: timestamp.Add(time.Minute)},
		{App: "ge", Timestamp: timestamp.Add(time.Second)},
	}
	insertDeploysAsEvents(deps, c)
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Header().Get("Content-Type"), check.Equals, "application/json")
	err = json.Unmarshal(recorder.Body.Bytes(), &result)
	c.Assert(err, check.IsNil)
	c.Assert(result, check.HasLen, 2)
	c.Assert(result[0].ID, check.NotNil)
	c.Assert(result[0].App, check.Equals, "g1")
	c.Assert(result[0].Timestamp.In(time.UTC), check.DeepEquals, timestamp.Add(time.Minute).In(time.UTC))
	c.Assert(result[1].App, check.Equals, "ge")
	c.Assert(result[1].Timestamp.In(time.UTC), check.DeepEquals, timestamp.Add(time.Second).In(time.UTC))
}

func (s *DeploySuite) TestDeployListByApp(c *check.C) {
	user, _ := s.token.User()
	a := app.App{Name: "myblog", Platform: "python", TeamOwner: s.team.Name}
	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	timestamp := time.Date(2013, time.November, 1, 0, 0, 0, 0, time.Local)
	deploys := []app.DeployData{
		{App: "myblog", Timestamp: timestamp},
		{App: "yourblog", Timestamp: timestamp},
	}
	insertDeploysAsEvents(deploys, c)
	recorder := httptest.NewRecorder()
	request, err := http.NewRequest("GET", "/deploys?app=myblog", nil)
	c.Assert(err, check.IsNil)
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Header().Get("Content-Type"), check.Equals, "application/json")
	var result []app.DeployData
	err = json.Unmarshal(recorder.Body.Bytes(), &result)
	c.Assert(err, check.IsNil)
	c.Assert(result, check.HasLen, 1)
	c.Assert(result[0].App, check.Equals, "myblog")
	c.Assert(result[0].Timestamp.In(time.UTC), check.DeepEquals, timestamp.In(time.UTC))
}

func (s *DeploySuite) TestDeployListByAppWithImage(c *check.C) {
	user, _ := s.token.User()
	a := app.App{Name: "myblog", Platform: "python", TeamOwner: s.team.Name}
	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	timestamp := time.Date(2013, time.November, 1, 0, 0, 0, 0, time.Local)
	deploys := []app.DeployData{
		{App: "myblog", Timestamp: timestamp, Image: "registry.tsuru.globoi.com/tsuru/app-example:v2", CanRollback: true},
		{App: "yourblog", Timestamp: timestamp, Image: "127.0.0.1:5000/tsuru/app-tsuru-dashboard:v1", CanRollback: true},
	}
	insertDeploysAsEvents(deploys, c)
	recorder := httptest.NewRecorder()
	request, err := http.NewRequest("GET", "/deploys?app=myblog", nil)
	c.Assert(err, check.IsNil)
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Header().Get("Content-Type"), check.Equals, "application/json")
	var result []app.DeployData
	err = json.Unmarshal(recorder.Body.Bytes(), &result)
	c.Assert(err, check.IsNil)
	c.Assert(result, check.HasLen, 1)
	c.Assert(result[0].Image, check.Equals, "v2")
	c.Assert(result[0].App, check.Equals, "myblog")
	c.Assert(result[0].Timestamp.In(time.UTC), check.DeepEquals, timestamp.In(time.UTC))
}

func (s *DeploySuite) TestDeployListAppWithNoDeploys(c *check.C) {
	user, _ := s.token.User()
	a := app.App{Name: "myblog", Platform: "python", TeamOwner: s.team.Name}
	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	recorder := httptest.NewRecorder()
	request, err := http.NewRequest("GET", "/deploys?app=myblog", nil)
	c.Assert(err, check.IsNil)
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusNoContent)
}

func (s *DeploySuite) TestDeployInfoByAdminUser(c *check.C) {
	a := app.App{Name: "g1", Platform: "python", TeamOwner: s.team.Name}
	user, _ := s.token.User()
	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	recorder := httptest.NewRecorder()
	timestamp := time.Now()
	depData := []app.DeployData{
		{App: "g1", Timestamp: timestamp.Add(-3600 * time.Second), Commit: "e293e3e3me03ejm3puejmp3ej3iejop32", Error: ""},
		{App: "g1", Timestamp: timestamp, Commit: "e82nn93nd93mm12o2ueh83dhbd3iu112", Error: ""},
	}
	lastDeploy := depData[1]
	lastDeploy.Origin = "git"
	evts := insertDeploysAsEvents(depData, c)
	url := fmt.Sprintf("/deploys/%s", evts[1].UniqueID.Hex())
	request, err := http.NewRequest("GET", url, nil)
	c.Assert(err, check.IsNil)
	token := customUserWithPermission(c, "myadmin", permission.Permission{
		Scheme:  permission.PermAppReadDeploy,
		Context: permission.Context(permission.CtxGlobal, ""),
	})
	request.Header.Set("Authorization", "bearer "+token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Header().Get("Content-Type"), check.Equals, "application/json")
	var result app.DeployData
	err = json.Unmarshal(recorder.Body.Bytes(), &result)
	c.Assert(err, check.IsNil)
	lastDeploy.ID = evts[1].UniqueID
	result.Timestamp = lastDeploy.Timestamp
	result.RemoveDate = lastDeploy.RemoveDate
	result.Duration = 0
	result.Log = ""
	c.Assert(result, check.DeepEquals, lastDeploy)
}

func (s *DeploySuite) TestDeployInfoDiff(c *check.C) {
	user, _ := s.token.User()
	a := app.App{Name: "g1", Platform: "python", TeamOwner: s.team.Name}
	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	recorder := httptest.NewRecorder()
	timestamp := time.Now()
	depData := []app.DeployData{
		{App: "g1", Timestamp: timestamp.Add(-3600 * time.Second), Commit: "e293e3e3me03ejm3puejmp3ej3iejop32", Error: "", Origin: "git"},
		{App: "g1", Timestamp: timestamp, Commit: "e82nn93nd93mm12o2ueh83dhbd3iu112", Error: "", Origin: "git", Diff: "fake-diff"},
	}
	lastDeploy := depData[1]
	evts := insertDeploysAsEvents(depData, c)
	url := fmt.Sprintf("/deploys/%s", evts[1].UniqueID.Hex())
	request, err := http.NewRequest("GET", url, nil)
	c.Assert(err, check.IsNil)
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Header().Get("Content-Type"), check.Equals, "application/json")
	lastDeploy.ID = evts[1].UniqueID
	var result app.DeployData
	err = json.Unmarshal(recorder.Body.Bytes(), &result)
	c.Assert(err, check.IsNil)
	result.Timestamp = lastDeploy.Timestamp
	result.RemoveDate = lastDeploy.RemoveDate
	result.Duration = 0
	result.Log = ""
	c.Assert(result, check.DeepEquals, lastDeploy)
}

func (s *DeploySuite) TestDeployInfoByNonAdminUser(c *check.C) {
	user, _ := s.token.User()
	a := app.App{Name: "g1", Platform: "python", TeamOwner: s.team.Name}
	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	user = &auth.User{Email: "user@user.com", Password: "123456"}
	app.AuthScheme = nativeScheme
	_, err = nativeScheme.Create(user)
	c.Assert(err, check.IsNil)
	team := &auth.Team{Name: "team"}
	err = s.conn.Teams().Insert(team)
	c.Assert(err, check.IsNil)
	token, err := nativeScheme.Login(map[string]string{"email": user.Email, "password": "123456"})
	c.Assert(err, check.IsNil)
	recorder := httptest.NewRecorder()
	timestamp := time.Now()
	depData := []app.DeployData{
		{App: "g1", Timestamp: timestamp.Add(-3600 * time.Second), Commit: "e293e3e3me03ejm3puejmp3ej3iejop32", Error: "", Origin: "git"},
		{App: "g1", Timestamp: timestamp, Commit: "e82nn93nd93mm12o2ueh83dhbd3iu112", Error: "", Origin: "git", Diff: "fake-diff"},
	}
	evts := insertDeploysAsEvents(depData, c)
	url := fmt.Sprintf("/deploys/%s", evts[1].UniqueID.Hex())
	request, err := http.NewRequest("GET", url, nil)
	c.Assert(err, check.IsNil)
	request.Header.Set("Authorization", "bearer "+token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusNotFound)
	body := recorder.Body.String()
	c.Assert(body, check.Equals, "Deploy not found.\n")
}

func (s *DeploySuite) TestDeployInfoByNonAuthenticated(c *check.C) {
	recorder := httptest.NewRecorder()
	url := fmt.Sprintf("/deploys/xpto")
	request, err := http.NewRequest("GET", url, nil)
	c.Assert(err, check.IsNil)
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusUnauthorized)
}

func (s *DeploySuite) TestDeployInfoByUserWithoutAccess(c *check.C) {
	user := &auth.User{Email: "user@user.com", Password: "123456"}
	app.AuthScheme = nativeScheme
	_, err := nativeScheme.Create(user)
	c.Assert(err, check.IsNil)
	team := &auth.Team{Name: "team"}
	err = s.conn.Teams().Insert(team)
	c.Assert(err, check.IsNil)
	a := app.App{Name: "g1", Platform: "python", TeamOwner: team.Name}
	err = app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	token, err := nativeScheme.Login(map[string]string{"email": user.Email, "password": "123456"})
	c.Assert(err, check.IsNil)
	recorder := httptest.NewRecorder()
	timestamp := time.Now()
	depData := []app.DeployData{
		{App: "g1", Timestamp: timestamp.Add(-3600 * time.Second), Commit: "e293e3e3me03ejm3puejmp3ej3iejop32", Error: "", Origin: "git"},
		{App: "g1", Timestamp: timestamp, Commit: "e82nn93nd93mm12o2ueh83dhbd3iu112", Error: "", Origin: "git", Diff: "fake-diff"},
	}
	evts := insertDeploysAsEvents(depData, c)
	url := fmt.Sprintf("/deploys/%s", evts[1].UniqueID.Hex())
	request, err := http.NewRequest("GET", url, nil)
	c.Assert(err, check.IsNil)
	request.Header.Set("Authorization", "bearer "+token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusNotFound)
	body := recorder.Body.String()
	c.Assert(body, check.Equals, "Deploy not found.\n")
}

func (s *DeploySuite) TestDeployRollbackHandler(c *check.C) {
	user, _ := s.token.User()
	a := app.App{Name: "otherapp", Platform: "python", TeamOwner: s.team.Name}
	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	v := url.Values{}
	v.Set("origin", "rollback")
	v.Set("image", "my-image-123:v1")
	u := fmt.Sprintf("/apps/%s/deploy/rollback", a.Name)
	request, err := http.NewRequest("POST", u, strings.NewReader(v.Encode()))
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Header().Get("Content-Type"), check.Equals, "application/x-json-stream")
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Body.String(), check.Equals, "{\"Message\":\"Rollback deploy called\"}\n")
	c.Assert(eventtest.EventDesc{
		Target: appTarget(a.Name),
		Owner:  s.token.GetUserName(),
		Kind:   "app.deploy",
		StartCustomData: map[string]interface{}{
			"app.name":   a.Name,
			"commit":     "",
			"filesize":   0,
			"kind":       "rollback",
			"archiveurl": "",
			"user":       s.token.GetUserName(),
			"image":      "my-image-123:v1",
			"origin":     "rollback",
			"build":      false,
			"rollback":   true,
		},
		EndCustomData: map[string]interface{}{
			"image": "my-image-123:v1",
		},
	}, eventtest.HasEvent)
}

func (s *DeploySuite) TestDeployRollbackHandlerWithCompleteImage(c *check.C) {
	user, _ := s.token.User()
	a := app.App{Name: "otherapp", Platform: "python", TeamOwner: s.team.Name}
	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	v := url.Values{}
	v.Set("origin", "rollback")
	v.Set("image", "127.0.0.1:5000/tsuru/app-tsuru-dashboard:v1")
	u := fmt.Sprintf("/apps/%s/deploy/rollback", a.Name)
	request, err := http.NewRequest("POST", u, strings.NewReader(v.Encode()))
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Header().Get("Content-Type"), check.Equals, "application/x-json-stream")
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Body.String(), check.Equals, "{\"Message\":\"Rollback deploy called\"}\n")
	c.Assert(eventtest.EventDesc{
		Target: appTarget(a.Name),
		Owner:  s.token.GetUserName(),
		Kind:   "app.deploy",
		StartCustomData: map[string]interface{}{
			"app.name":   a.Name,
			"commit":     "",
			"filesize":   0,
			"kind":       "rollback",
			"archiveurl": "",
			"user":       s.token.GetUserName(),
			"image":      "127.0.0.1:5000/tsuru/app-tsuru-dashboard:v1",
			"origin":     "rollback",
			"build":      false,
			"rollback":   true,
		},
		EndCustomData: map[string]interface{}{
			"image": "127.0.0.1:5000/tsuru/app-tsuru-dashboard:v1",
		},
	}, eventtest.HasEvent)
}

func (s *DeploySuite) TestDeployRollbackHandlerWithOnlyVersionImage(c *check.C) {
	user, _ := s.token.User()
	a := app.App{Name: "otherapp", Platform: "python", TeamOwner: s.team.Name}
	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName("otherapp", "127.0.0.1:5000/tsuru/app-tsuru-dashboard:v1")
	c.Assert(err, check.IsNil)
	v := url.Values{}
	v.Set("origin", "rollback")
	v.Set("image", "v1")
	u := fmt.Sprintf("/apps/%s/deploy/rollback", a.Name)
	request, err := http.NewRequest("POST", u, strings.NewReader(v.Encode()))
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Header().Get("Content-Type"), check.Equals, "application/x-json-stream")
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Body.String(), check.Equals, "{\"Message\":\"Rollback deploy called\"}\n")
	c.Assert(eventtest.EventDesc{
		Target: appTarget(a.Name),
		Owner:  s.token.GetUserName(),
		Kind:   "app.deploy",
		StartCustomData: map[string]interface{}{
			"app.name":   a.Name,
			"commit":     "",
			"filesize":   0,
			"kind":       "rollback",
			"archiveurl": "",
			"user":       s.token.GetUserName(),
			"image":      "v1",
			"origin":     "rollback",
			"build":      false,
			"rollback":   true,
		},
		EndCustomData: map[string]interface{}{
			"image": "127.0.0.1:5000/tsuru/app-tsuru-dashboard:v1",
		},
		LogMatches: `Rollback deploy called`,
	}, eventtest.HasEvent)
}

func (s *DeploySuite) TestDeployRollbackHandlerWithInexistVersion(c *check.C) {
	user, _ := s.token.User()
	a := app.App{Name: "otherapp", Platform: "python", TeamOwner: s.team.Name}
	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	b := app.App{Name: "otherapp2", Platform: "python", TeamOwner: s.team.Name}
	err = app.CreateApp(&b, user)
	c.Assert(err, check.IsNil)
	v := url.Values{}
	v.Set("origin", "rollback")
	v.Set("image", "v3")
	u := fmt.Sprintf("/apps/%s/deploy/rollback", a.Name)
	request, err := http.NewRequest("POST", u, strings.NewReader(v.Encode()))
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	server := RunServer(true)
	server.ServeHTTP(recorder, request)
	c.Assert(recorder.Header().Get("Content-Type"), check.Equals, "application/x-json-stream")
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	var body map[string]string
	err = json.Unmarshal(recorder.Body.Bytes(), &body)
	c.Assert(err, check.IsNil)
	c.Assert(body, check.DeepEquals, map[string]string{"Message": "", "Error": `invalid version: "v3"`})
}

func (s *DeploySuite) TestDiffDeploy(c *check.C) {
	diff := `--- hello.go	2015-11-25 16:04:22.409241045 +0000
+++ hello.go	2015-11-18 18:40:21.385697080 +0000
@@ -1,10 +1,7 @@
 package main

-import (
-    "fmt"
-)
+import "fmt"

-func main() {
-	fmt.Println("Hello")
+func main2() {
+	fmt.Println("Hello World!")
 }
`
	user, _ := s.token.User()
	a := app.App{Name: "otherapp", Platform: "python", TeamOwner: s.team.Name}
	err := app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	v := url.Values{}
	v.Set("customdata", diff)
	body := strings.NewReader(v.Encode())
	url := fmt.Sprintf("/apps/%s/diff", a.Name)
	request, err := http.NewRequest("POST", url, body)
	c.Assert(err, check.IsNil)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", "bearer "+s.token.GetValue())
	evt, err := event.New(&event.Opts{
		Target:  appTarget(a.Name),
		Kind:    permission.PermAppDeploy,
		Owner:   s.token,
		Allowed: event.Allowed(permission.PermAppReadEvents, contextsForApp(&a)...),
	})
	c.Assert(err, check.IsNil)
	recorder := httptest.NewRecorder()
	m := RunServer(true)
	m.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	c.Assert(recorder.Body.String(), check.Equals, "Saving the difference between the old and new code\n")
	err = evt.Done(nil)
	c.Assert(err, check.IsNil)
	c.Assert(eventtest.EventDesc{
		Target: appTarget(a.Name),
		Owner:  s.token.GetUserName(),
		Kind:   "app.deploy",
	}, eventtest.HasEvent)
}

func (s *DeploySuite) TestDiffDeployWhenUserDoesNotHaveAccessToApp(c *check.C) {
	diff := `--- hello.go	2015-11-25 16:04:22.409241045 +0000
+++ hello.go	2015-11-18 18:40:21.385697080 +0000
@@ -1,10 +1,7 @@
 package main

-import (
-    "fmt"
-)
+import "fmt"

-func main() {
-	fmt.Println("Hello")
+func main2() {
+	fmt.Println("Hello World!")
 }
	`

	user, _ := s.token.User()
	user1 := &auth.User{Email: "someone@tsuru.io", Password: "user123"}
	_, err := nativeScheme.Create(user1)
	c.Assert(err, check.IsNil)
	token, err := nativeScheme.Login(map[string]string{"email": user1.Email, "password": "user123"})
	c.Assert(err, check.IsNil)
	a := app.App{Name: "otherapp", Platform: "python", TeamOwner: s.team.Name}
	err = app.CreateApp(&a, user)
	c.Assert(err, check.IsNil)
	v := url.Values{}
	v.Set("customdata", diff)
	body := strings.NewReader(v.Encode())
	url := fmt.Sprintf("/apps/%s/diff?:appname=%s", a.Name, a.Name)
	request, err := http.NewRequest("POST", url, body)
	c.Assert(err, check.IsNil)
	request.Header.Set("Authorization", "bearer "+token.GetValue())
	recorder := httptest.NewRecorder()
	m := RunServer(true)
	m.ServeHTTP(recorder, request)
	c.Assert(recorder.Code, check.Equals, http.StatusOK)
	expected := `Saving the difference between the old and new code
`
	c.Assert(recorder.Body.String(), check.Equals, expected+permission.ErrUnauthorized.Error()+"\n")
}
