package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

const (
	KeyLogLevel = "log.level"

	KeyAgentLogFormat = "run.log-format"
	KeyNumIOThreads   = "run.num-io-threads"
	KeyPProfEnable    = "run.pprof.enable"
	KeyPProfPort      = "run.pprof.port"
	KeyRetryDBInit    = "run.retry-db-init"
	KeyAgentUseColor  = "run.use-color"

	KeyPGData         = "postgresql.pgdata"
	KeyPGDatabase     = "postgresql.database"
	KeyPGHost         = "postgresql.host"
	KeyPGMode         = "postgresql.mode"
	KeyPGPassword     = "postgresql.password"
	KeyPGPollInterval = "postgresql.poll-interval"
	KeyPGPort         = "postgresql.port"
	KeyPGUser         = "postgresql.user"

	KeyWALReadahead = "postgresql.wal.readahead-bytes"
	KeyWALThreads   = "postgresql.wal.threads"

	KeyXLogMode = "postgresql.xlog.mode"
	KeyXLogPath = "postgresql.xlog.pg_waldump-path"
)

const (
	MetricsSysPreadLatency = "ioc-sys-pread-ms"
	MetricsPrefaultCount   = "ioc-prefault-count"
	MetricsIOCacheHit      = "ioc-hit"
	MetricsIOCacheMiss     = "ioc-miss"

	MetricsSysCloseCount     = "fh-sys-close-count"
	MetricsSysOpenCount      = "fh-sys-open-count"
	MetricsSysOpenLatency    = "fh-sys-open-us"
	MetricsSysPreadBytes     = "fh-sys-pread-bytes"
	MetricsWalDumpErrorCount = "fh-waldump-error-count"

	MetricsWALFaultCount        = "wal-file-fault-count"
	MetricsWALFaultTime         = "wal-file-fault-time"
	MetricsWalDumpLen           = "wal-waldump-out-len"
	MetricsWalDumpBlocksMatched = "wal-waldump-blocks-matched"
	MetricsWalDumpLinesMatched  = "wal-waldump-lines-matched"
	MetricsWalDumpLinesScanned  = "wal-waldump-lines-scanned"
	MetricsXLogPrefaulted       = "wal-xlog-prefaulted-count"
)

const (
	// Use a log format that resembles time.RFC3339Nano but includes all trailing
	// zeros so that we get fixed-width logging.
	LogTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

	// 8601 Extended Format: YYYY-MM-DDTHH:mm:ss.sssZ
	LogTimeFormatBunyan = "2006-01-02T15:04:05.000Z"

	StatsInterval = 60 * time.Second
)

type LogFormat uint

const (
	LogFormatAuto LogFormat = iota
	LogFormatZerolog
	LogFormatBunyan
	LogFormatHuman
)

func (f LogFormat) String() string {
	switch f {
	case LogFormatAuto:
		return "auto"
	case LogFormatZerolog:
		return "zerolog"
	case LogFormatBunyan:
		return "bunyan"
	case LogFormatHuman:
		return "human"
	default:
		panic(fmt.Sprintf("unknown log format: %d", f))
	}
}

func LogLevelParse(s string) (LogFormat, error) {
	switch logFormat := strings.ToLower(viper.GetString(KeyAgentLogFormat)); logFormat {
	case "auto":
		return LogFormatAuto, nil
	case "json", "zerolog":
		return LogFormatZerolog, nil
	case "bunyan":
		return LogFormatBunyan, nil
	case "human":
		return LogFormatHuman, nil
	default:
		return LogFormatAuto, fmt.Errorf("unsupported log format: %q", logFormat)
	}
}
