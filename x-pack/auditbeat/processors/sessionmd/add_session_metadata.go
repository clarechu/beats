// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

//go:build linux

package sessionmd

import (
	"context"
	"fmt"
	"reflect"
	"strconv"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/beats/v7/libbeat/common/cfgwarn"
	"github.com/elastic/beats/v7/libbeat/processors"
	"github.com/elastic/beats/v7/x-pack/auditbeat/processors/sessionmd/processdb"
	"github.com/elastic/beats/v7/x-pack/auditbeat/processors/sessionmd/procfs"
	"github.com/elastic/beats/v7/x-pack/auditbeat/processors/sessionmd/provider"
	"github.com/elastic/beats/v7/x-pack/auditbeat/processors/sessionmd/provider/kerneltracingprovider"
	"github.com/elastic/beats/v7/x-pack/auditbeat/processors/sessionmd/provider/procfsprovider"
	"github.com/elastic/beats/v7/x-pack/auditbeat/processors/sessionmd/types"
	cfg "github.com/elastic/elastic-agent-libs/config"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/mapstr"
)

const (
	processorName = "add_session_metadata"
	logName       = "processor." + processorName
)

// InitializeModule initializes this module.
func InitializeModule() {
	processors.RegisterPlugin(processorName, New)
}

type addSessionMetadata struct {
	ctx      context.Context
	cancel   context.CancelFunc
	config   config
	logger   *logp.Logger
	db       *processdb.DB
	provider provider.Provider
	backend  string
}

func New(cfg *cfg.C) (beat.Processor, error) {
	cfgwarn.Beta("add_session_metadata processor is a beta feature.")
	c := defaultConfig()
	if err := cfg.Unpack(&c); err != nil {
		return nil, fmt.Errorf("fail to unpack the %v configuration: %w", processorName, err)
	}

	logger := logp.NewLogger(logName)

	ctx, cancel := context.WithCancel(context.Background())
	reader := procfs.NewProcfsReader(*logger)
	db, err := processdb.NewDB(reader, *logger)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create DB: %w", err)
	}

	if c.Backend != "kernel_tracing" {
		backfilledPIDs := db.ScrapeProcfs()
		logger.Infof("backfilled %d processes", len(backfilledPIDs))
	}

	var p provider.Provider

	switch c.Backend {
	case "auto":
		p, err = kerneltracingprovider.NewProvider(ctx, logger)
		if err != nil {
			// Most likely cause of error is not supporting ebpf or kprobes on system, try procfs
			p, err = procfsprovider.NewProvider(ctx, logger, db, reader, c.PIDField)
			if err != nil {
				cancel()
				return nil, fmt.Errorf("failed to create provider: %w", err)
			}
			logger.Info("backend=auto using procfs")
		} else {
			logger.Info("backend=auto using kernel_tracing")
		}
	case "procfs":
		p, err = procfsprovider.NewProvider(ctx, logger, db, reader, c.PIDField)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("failed to create procfs provider: %w", err)
		}
	case "kernel_tracing":
		p, err = kerneltracingprovider.NewProvider(ctx, logger)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("failed to create kernel_tracing provider: %w", err)
		}
	default:
		cancel()
		return nil, fmt.Errorf("unknown backend configuration")
	}
	return &addSessionMetadata{
		ctx:      ctx,
		cancel:   cancel,
		config:   c,
		logger:   logger,
		db:       db,
		provider: p,
		backend:  c.Backend,
	}, nil
}

func (p *addSessionMetadata) Run(ev *beat.Event) (*beat.Event, error) {
	pi, err := ev.GetValue(p.config.PIDField)
	if err != nil {
		// Do not attempt to enrich events without PID; it's not a supported event
		return ev, nil //nolint:nilerr // Running on events without PID is expected
	}

	// Do not enrich failed syscalls, as there was no actual process change related to it
	v, err := ev.GetValue("auditd.result")
	if err == nil && v == "fail" {
		return ev, nil
	}

	pid, err := pidToUInt32(pi)
	if err != nil {
		return ev, nil //nolint:nilerr // Running on events with a different PID type is not a processor error
	}

	err = p.provider.SyncDB(ev, pid)
	if err != nil {
		return ev, err
	}

	result, err := p.enrich(ev)
	if err != nil {
		return ev, fmt.Errorf("enriching event: %w", err)
	}
	return result, nil
}

func (p *addSessionMetadata) Close() error {
	p.db.Close()
	p.cancel()
	return nil
}

func (p *addSessionMetadata) String() string {
	return fmt.Sprintf("%v=[backend=%s, pid_field=%s]",
		processorName, p.config.Backend, p.config.PIDField)
}

func (p *addSessionMetadata) enrich(ev *beat.Event) (*beat.Event, error) {
	pidIf, err := ev.GetValue(p.config.PIDField)
	if err != nil {
		return nil, err
	}
	pid, err := pidToUInt32(pidIf)
	if err != nil {
		return nil, fmt.Errorf("cannot parse pid field '%s': %w", p.config.PIDField, err)
	}

	var fullProcess types.Process
	if p.backend == "kernel_tracing" {
		// kernel_tracing doesn't enrich with the processor DB;  process info is taken directly from quark cache
		proc, err := p.provider.GetProcess(pid)
		if err != nil {
			e := fmt.Errorf("pid %v not found in db: %w", pid, err)
			p.logger.Warnw("PID not found in provider", "pid", pid, "error", err)
			return nil, e
		}
		fullProcess = *proc
	} else {
		fullProcess, err = p.db.GetProcess(pid)
		if err != nil {
			e := fmt.Errorf("pid %v not found in db: %w", pid, err)
			p.logger.Warnw("PID not found in provider", "pid", pid, "error", err)
			return nil, e
		}
	}
	processMap := fullProcess.ToMap()

	if b, err := ev.Fields.HasKey("process"); !b || err != nil {
		return nil, fmt.Errorf("no process field in event")
	}
	m, ok := tryToMapStr(ev.Fields["process"])
	if !ok {
		return nil, fmt.Errorf("process field type not supported")
	}

	result := ev.Clone()
	err = mapstr.MergeFieldsDeep(m, processMap, true)
	if err != nil {
		return nil, fmt.Errorf("merging enriched fields with event: %w", err)
	}
	result.Fields["process"] = m
	return result, nil
}

// pidToUInt32 converts PID value to uint32
func pidToUInt32(value interface{}) (pid uint32, err error) {
	switch v := value.(type) {
	case string:
		nr, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("error converting string to integer: %w", err)
		}
		pid = uint32(nr)
	case uint32:
		pid = v
	case int, int8, int16, int32, int64:
		pid64 := reflect.ValueOf(v).Int()
		if pid = uint32(pid64); int64(pid) != pid64 {
			return 0, fmt.Errorf("integer out of range: %d", pid64)
		}
	case uint, uintptr, uint8, uint16, uint64:
		pidu64 := reflect.ValueOf(v).Uint()
		if pid = uint32(pidu64); uint64(pid) != pidu64 {
			return 0, fmt.Errorf("integer out of range: %d", pidu64)
		}
	default:
		return 0, fmt.Errorf("not an integer or string, but %T", v)
	}
	return pid, nil
}

func tryToMapStr(v interface{}) (mapstr.M, bool) {
	switch m := v.(type) {
	case mapstr.M:
		return m, true
	case map[string]interface{}:
		return mapstr.M(m), true
	default:
		return nil, false
	}
}
