// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package winlog

import (
	"errors"
	"fmt"
	"io"
	"time"

	input "github.com/elastic/beats/v7/filebeat/input/v2"
	cursor "github.com/elastic/beats/v7/filebeat/input/v2/input-cursor"
	"github.com/elastic/beats/v7/libbeat/feature"
	"github.com/elastic/beats/v7/libbeat/management/status"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/go-concert/ctxtool"
	"github.com/elastic/go-concert/timed"

	"github.com/elastic/beats/v7/winlogbeat/checkpoint"
	"github.com/elastic/beats/v7/winlogbeat/eventlog"
	conf "github.com/elastic/elastic-agent-libs/config"
)

type eventlogRunner struct{}

const pluginName = "winlog"

const channelNotFoundError = "Encountered channel not found error when opening Windows Event Log"
const eventLogReadingError = "Error occurred while reading from Windows Event Log"
const resetError = "Error resetting Windows Event Log handle"

// Plugin create a stateful input Plugin collecting logs from Windows Event Logs.
func Plugin(log *logp.Logger, store cursor.StateStore) input.Plugin {
	return input.Plugin{
		Name:       pluginName,
		Stability:  feature.Beta,
		Deprecated: false,
		Info:       "Windows Event Logs",
		Doc:        "The winlog input collects logs from the local windows event log service",
		Manager: &cursor.InputManager{
			Logger:     log,
			StateStore: store,
			Type:       pluginName,
			Configure:  configure,
		},
	}
}

func configure(cfg *conf.C) ([]cursor.Source, cursor.Input, error) {
	var cfgs []*conf.C
	if cfg.HasField("event_logs") {
		multiCfg := struct {
			EventLogs []*conf.C `config:"event_logs"`
		}{}
		if err := cfg.Unpack(&multiCfg); err != nil {
			return nil, nil, err
		}
		cfgs = multiCfg.EventLogs
	} else {
		// TODO: Single event log configurations are accepted to support
		// integration configs, it is an undocumented feature to avoid
		// confusion. Should we remove this in the future?
		cfgs = append(cfgs, cfg)
	}

	sources := make([]cursor.Source, len(cfgs))
	for i, cfg := range cfgs {
		eventLog, err := eventlog.New(cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create new event log. %w", err)
		}
		sources[i] = eventLog
	}

	return sources, eventlogRunner{}, nil
}

func (eventlogRunner) Name() string { return pluginName }

func (eventlogRunner) Test(source cursor.Source, ctx input.TestContext) error {
	api := source.(eventlog.EventLog)
	err := api.Open(checkpoint.EventLogState{})
	if err != nil {
		return fmt.Errorf("failed to open %q: %w", api.Channel(), err)
	}
	return api.Close()
}

func (eventlogRunner) Run(
	ctx input.Context,
	source cursor.Source,
	cursor cursor.Cursor,
	publisher cursor.Publisher,
) error {
	api := source.(eventlog.EventLog)
	log := ctx.Logger.With("eventlog", source.Name(), "channel", api.Channel())

	// setup closing the API if either the run function is signaled asynchronously
	// to shut down or when returning after io.EOF
	cancelCtx, cancelFn := ctxtool.WithFunc(ctx.Cancelation, func() {
		if err := api.Close(); err != nil {
			log.Errorw("Error while closing Windows Event Log access", "error", err)
		}
	})
	defer cancelFn()

	// Flag used to detect repeat "channel not found" errors, eliminating log spam.
	channelNotFoundErrDetected := false
	ctx.UpdateStatus(status.Running, "")

runLoop:
	for {
		//nolint:nilerr // only log error if we are not shutting down
		if cancelCtx.Err() != nil {
			return nil
		}

		evtCheckpoint := initCheckpoint(log, cursor)
		openErr := api.Open(evtCheckpoint)
		// Mark the input running.
		// Status will be changed to "Degraded" if any error are encountered during opening/reading
		ctx.UpdateStatus(status.Running, "")

		switch {
		case eventlog.IsRecoverable(openErr):
			log.Errorw("Encountered recoverable error when opening Windows Event Log", "error", openErr)
			_ = timed.Wait(cancelCtx, 5*time.Second)
			continue
		case !api.IsFile() && eventlog.IsChannelNotFound(openErr):
			if !channelNotFoundErrDetected {
				log.Errorw(channelNotFoundError, "error", openErr)
			} else {
				log.Debugw(channelNotFoundError, "error", openErr)
			}
			ctx.UpdateStatus(status.Degraded, fmt.Sprintf("%s: %v", channelNotFoundError, openErr))
			channelNotFoundErrDetected = true
			_ = timed.Wait(cancelCtx, 5*time.Second)
			continue
		case openErr != nil:
			ctx.UpdateStatus(status.Degraded, fmt.Sprintf("failed to open Windows Event Log channel %q: %v", api.Channel(), openErr))
			return fmt.Errorf("failed to open Windows Event Log channel %q: %w", api.Channel(), openErr)
		}
		channelNotFoundErrDetected = false

		log.Debug("Windows Event Log opened successfully")

		// read loop
		for cancelCtx.Err() == nil {
			records, err := api.Read()
			if eventlog.IsRecoverable(err) {
				log.Errorw("Encountered recoverable error when reading from Windows Event Log", "error", err)
				if resetErr := api.Reset(); resetErr != nil {
					log.Errorw(resetError, "error", resetErr)
					ctx.UpdateStatus(status.Degraded, fmt.Sprintf("%s: %v", resetError, resetErr))
				}
				continue runLoop
			}
			if !api.IsFile() && eventlog.IsChannelNotFound(err) {
				log.Errorw("Encountered channel not found error when reading from Windows Event Log", "error", err)
				if resetErr := api.Reset(); resetErr != nil {
					log.Errorw(resetError, "error", resetErr)
					ctx.UpdateStatus(status.Degraded, fmt.Sprintf("%s: %v", resetError, resetErr))
				}
				continue runLoop
			}

			if err != nil {
				if errors.Is(err, io.EOF) {
					log.Debugw("End of Winlog event stream reached", "error", err)
					return nil
				}

				//nolint:nilerr // only log error if we are not shutting down
				if cancelCtx.Err() != nil {
					return nil
				}

				log.Errorw(eventLogReadingError, "error", err)
				ctx.UpdateStatus(status.Degraded, fmt.Sprintf("%s: %v", eventLogReadingError, err))
				return err
			}
			if len(records) == 0 {
				_ = timed.Wait(cancelCtx, time.Second)
				continue
			}

			for _, record := range records {
				event := record.ToEvent()
				if err := publisher.Publish(event, record.Offset); err != nil {
					// Publisher indicates disconnect when returning an error.
					// stop trying to publish records and quit
					ctx.UpdateStatus(status.Degraded, fmt.Sprintf("Error occurred while publishing from winlog: %v", err))
					return err
				}
			}
		}
	}
}

func initCheckpoint(log *logp.Logger, cursor cursor.Cursor) checkpoint.EventLogState {
	var cp checkpoint.EventLogState
	if cursor.IsNew() {
		return cp
	}

	if err := cursor.Unpack(&cp); err != nil {
		log.Errorf("Reset winlog position. Failed to read checkpoint from registry: %v", err)
		return checkpoint.EventLogState{}
	}

	return cp
}
