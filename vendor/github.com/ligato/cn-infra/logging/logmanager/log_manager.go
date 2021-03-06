// Copyright (c) 2017 Cisco and/or its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package logmanager

import (
	"fmt"
	"github.com/gorilla/mux"
	"github.com/ligato/cn-infra/httpmux"
	"github.com/ligato/cn-infra/logging"
	"github.com/unrolled/render"
	"net/http"
)

// LoggerData encapsulates parameters of a logger represented as strings.
type LoggerData struct {
	Logger string `json:"logger"`
	Level  string `json:"level"`
}

// Variable names in logger registry URLs
const (
	loggerVarName = "logger"
	levelVarName  = "level"
)

// Plugin allows to manage log levels of the loggers using HTTP.
type Plugin struct {
	ManagedLoggers logging.LogManagement
	HTTP           *httpmux.Plugin

	logging.Logger
}

// Init is called at plugin initialization. It register the following handlers:
// - List all registered loggers:
//   > curl -X GET http://localhost:<port>/log/list
// - Set log level for a registered logger:
//   > curl -X PUT http://localhost:<port>/log/<logger-name>/<log-level>
func (lm *Plugin) Init() error {
	var err error
	lm.Logger, err = lm.ManagedLoggers.NewLogger("LogManager")
	if err != nil {
		return err
	}
	lm.HTTP.RegisterHTTPHandler(fmt.Sprintf("/log/{%s}/{%s:debug|info|warning|error|fatal|panic}",
		loggerVarName, levelVarName), lm.logLevelHandler, "PUT")
	lm.HTTP.RegisterHTTPHandler("/log/list", lm.listLoggersHandler, "GET")
	return nil
}

// Close is called at plugin cleanup phase.
func (lm *Plugin) Close() error {
	return nil
}

// ListLoggers lists all registered loggers.
func (lm *Plugin) listLoggers() []LoggerData {
	loggers := []LoggerData{}

	lgs := lm.ManagedLoggers.Registry().ListLoggers()
	for lg, lvl := range lgs {
		ld := LoggerData{
			Logger: lg,
			Level:  lvl,
		}
		loggers = append(loggers, ld)
	}

	return loggers
}

// setLoggerLogLevel modifies the log level of the all loggers in a plugin
func (lm *Plugin) setLoggerLogLevel(name string, level string) error {
	lm.Debugf("SetLogLevel name '%s', level '%s'", name, level)

	return lm.ManagedLoggers.Registry().SetLevel(name, level)
}

// logLevelHandler processes requests to set log level on loggers in a plugin
func (lm *Plugin) logLevelHandler(formatter *render.Render) http.HandlerFunc {

	return func(w http.ResponseWriter, req *http.Request) {
		lm.Infof("Path: %s", req.URL.Path)
		vars := mux.Vars(req)
		if vars == nil {
			formatter.JSON(w, http.StatusNotFound, struct{}{})
			return
		}
		err := lm.setLoggerLogLevel(vars[loggerVarName], vars[levelVarName])
		if err != nil {
			formatter.JSON(w, http.StatusNotFound,
				struct{ Error string }{err.Error()})
			return
		}
		formatter.JSON(w, http.StatusOK,
			LoggerData{Logger: vars[loggerVarName], Level: vars[levelVarName]})
	}
}

// listLoggersHandler processes requests to list all registered loggers
func (lm *Plugin) listLoggersHandler(formatter *render.Render) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		formatter.JSON(w, http.StatusOK, lm.listLoggers())
	}
}
