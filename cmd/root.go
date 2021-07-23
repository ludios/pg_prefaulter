// Copyright © 2017 Sean Chittenden <sean@chittenden.org>
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	_ "expvar"
	"fmt"
	"io"
	stdlog "log"
	"net/http"
	"os"
	"strings"

	"github.com/bschofield/pg_prefaulter/buildtime"
	"github.com/bschofield/pg_prefaulter/config"
	isatty "github.com/mattn/go-isatty"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// CLI flags
var (
	cfgFile string
)

// RootCmd represents the base command when called without any subcommands
var RootCmd = &cobra.Command{
	Use:   buildtime.PROGNAME,
	Short: buildtime.PROGNAME + `pre-faults PostgreSQL heap pages based on WAL files`,
	Long: `
PostgreSQL's WAL-receiver applies WAL files in serial.  This design implicitly
assumes that the heap page required to apply the WAL entry is within the
operating system's filesystem cache.  If the filesystem cache does not contain
the necessary heap page, the PostgreSQL WAL apply process will be block while
the OS faults in the page from its storage.  For large working sets of data or
when the filesystem cache is cold, this is problematic for streaming replicas
because they will lag and fall behind.

` + buildtime.PROGNAME + `(1) mitigates this serially scheduled IO problem by
reading WAL entries via pg_waldump(1) and performing parallel pread(2) calls in
order to "pre-fault" the page into the OS's filesystem cache so that when the
PostgreSQL WAL receiver goes to apply a WAL entry to its heap, the page is
already loaded into the OS'es filesystem cache.

`,

	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Re-initialize logging with user-supplied configuration parameters
		{
			// os.Stdout isn't guaranteed to be thread-safe, wrap in a sync writer.
			// Files are guaranteed to be safe, terminals are not.
			var logWriter io.Writer
			if isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()) {
				logWriter = zerolog.SyncWriter(os.Stdout)
			} else {
				logWriter = os.Stdout
			}

			logFmt, err := config.LogLevelParse(viper.GetString(config.KeyAgentLogFormat))
			if err != nil {
				return errors.Wrap(err, "unable to parse log format")
			}

			if logFmt == config.LogFormatAuto {
				if isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()) {
					logFmt = config.LogFormatHuman
				} else {
					logFmt = config.LogFormatZerolog
				}
			}

			var zlog zerolog.Logger
			switch logFmt {
			case config.LogFormatZerolog:
				zlog = zerolog.New(logWriter).With().Timestamp().Logger()
			case config.LogFormatBunyan:
				hostname, err := os.Hostname()
				switch {
				case err != nil:
					return errors.Wrap(err, "unable to determine the hostname")
				case hostname == "":
					return fmt.Errorf("unable to use bunyan logging with an empty hostname")
				}

				zerolog.LevelFieldName = "level"

				zerolog.TimeFieldFormat = config.LogTimeFormatBunyan
				zerolog.TimestampFieldName = "time"
				zerolog.MessageFieldName = "msg"

				zlog = zerolog.New(logWriter).With().
					Timestamp().
					Int("v", 0). // Bunyan version
					Str("name", buildtime.PROGNAME).
					Str("hostname", hostname).
					Int("pid", os.Getpid()).
					Logger()
			case config.LogFormatHuman:
				useColor := viper.GetBool(config.KeyAgentUseColor)
				w := zerolog.ConsoleWriter{
					Out:     logWriter,
					NoColor: !useColor,
				}
				zlog = zerolog.New(w).With().Timestamp().Logger()
			default:
				return fmt.Errorf("unsupported log format: %q", logFmt)
			}

			log.Logger = zlog

			stdlog.SetFlags(0)
			stdlog.SetOutput(zlog)
		}

		// Perform input validation

		switch logLevel := strings.ToUpper(viper.GetString(config.KeyLogLevel)); logLevel {
		case "DEBUG":
			zerolog.SetGlobalLevel(zerolog.DebugLevel)
		case "INFO":
			zerolog.SetGlobalLevel(zerolog.InfoLevel)
		case "WARN":
			zerolog.SetGlobalLevel(zerolog.WarnLevel)
		case "ERROR":
			zerolog.SetGlobalLevel(zerolog.ErrorLevel)
		case "FATAL":
			zerolog.SetGlobalLevel(zerolog.FatalLevel)
		default:
			// FIXME(seanc@): move the supported log levels into a global constant
			return fmt.Errorf("unsupported error level: %q (supported levels: %s)", logLevel,
				strings.Join([]string{"DEBUG", "INFO", "WARN", "ERROR", "FATAL"}, " "))
		}

		go func() {
			if !viper.GetBool(config.KeyPProfEnable) {
				log.Debug().Msg("pprof endpoint disabled by request")
				return
			}

			pprofPort := viper.GetInt(config.KeyPProfPort)
			log.Debug().Int("pprof-port", pprofPort).Msg("starting pprof endpoing agent")
			if err := http.ListenAndServe(fmt.Sprintf("localhost:%d", pprofPort), nil); err != nil {
				log.Fatal().Err(err).Msg("unable to start the pprof listener")
			}
		}()

		return nil
	},
}

func Execute() {
	if err := RootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	zerolog.TimeFieldFormat = config.LogTimeFormat
	zerolog.SetGlobalLevel(zerolog.DebugLevel)

	// os.Stderr isn't guaranteed to be thread-safe, wrap in a sync writer.  Files
	// are guaranteed to be safe, terminals are not.
	zlog := zerolog.New(zerolog.SyncWriter(os.Stderr)).With().Timestamp().Logger()
	log.Logger = zlog

	stdlog.SetFlags(0)
	stdlog.SetOutput(zlog)

	RootCmd.PersistentFlags().StringVar(&cfgFile, "config", buildtime.PROGNAME+`.toml`, "config file")

	{
		const (
			key          = config.KeyLogLevel
			longName     = "log-level"
			shortName    = "l"
			defaultValue = "INFO"
			description  = "Log level"
		)

		RootCmd.PersistentFlags().StringP(longName, shortName, defaultValue, description)
		viper.BindPFlag(key, RootCmd.PersistentFlags().Lookup(longName))
		viper.SetDefault(key, defaultValue)
	}

	{
		const (
			key         = config.KeyAgentLogFormat
			longName    = "log-format"
			shortName   = "F"
			description = `Specify the log format ("auto", "zerolog", "human", or "bunyan")`
		)

		defaultValue := config.LogFormatAuto.String()
		RootCmd.PersistentFlags().StringP(longName, shortName, defaultValue, description)
		viper.BindPFlag(key, RootCmd.PersistentFlags().Lookup(longName))
		viper.SetDefault(key, defaultValue)
	}

	{
		const (
			key         = config.KeyAgentUseColor
			longName    = "use-color"
			shortName   = "C"
			description = "Use ASCII colors"
		)

		defaultValue := false
		if isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()) {
			defaultValue = true
		}

		RootCmd.PersistentFlags().BoolP(longName, shortName, defaultValue, description)
		viper.BindPFlag(key, RootCmd.PersistentFlags().Lookup(longName))
		viper.SetDefault(key, defaultValue)
	}

	{
		const (
			key          = config.KeyPGData
			longName     = "pgdata"
			shortName    = "D"
			defaultValue = "pgdata"
			envVar       = "PGDATA"
			description  = "Path to PGDATA"
		)

		RootCmd.PersistentFlags().StringP(longName, shortName, defaultValue, description)
		viper.BindPFlag(key, RootCmd.PersistentFlags().Lookup(longName))
		viper.BindEnv(key, envVar)
		viper.SetDefault(key, defaultValue)
	}

	{
		const (
			key          = config.KeyPGHost
			longName     = "hostname"
			shortName    = "H"
			defaultValue = "/tmp"
			envVar       = "PGHOST"
			description  = "Hostname to connect to PostgreSQL"
		)

		RootCmd.PersistentFlags().StringP(longName, shortName, defaultValue, description)
		viper.BindPFlag(key, RootCmd.PersistentFlags().Lookup(longName))
		viper.BindEnv(key, envVar)
		viper.SetDefault(key, defaultValue)
	}

	{
		const (
			key          = config.KeyPGPort
			longName     = "port"
			shortName    = "p"
			defaultValue = 5432
			envVar       = "PGPORT"
			description  = "Hostname to connect to PostgreSQL"
		)

		RootCmd.PersistentFlags().UintP(longName, shortName, defaultValue, description)
		viper.BindPFlag(key, RootCmd.PersistentFlags().Lookup(longName))
		viper.BindEnv(key, envVar)
		viper.SetDefault(key, defaultValue)
	}

	{
		const (
			key          = config.KeyPGDatabase
			longName     = "database"
			shortName    = "d"
			defaultValue = "postgres"
			envVar       = "PGDATABASE"
			description  = "Database name to connect to"
		)

		RootCmd.PersistentFlags().StringP(longName, shortName, defaultValue, defaultValue)
		viper.BindPFlag(key, RootCmd.PersistentFlags().Lookup(longName))
		viper.BindEnv(key, envVar)
		viper.SetDefault(key, defaultValue)
	}

	{
		const (
			key          = config.KeyPGUser
			longName     = "username"
			shortName    = "U"
			defaultValue = "postgres"
			envVar       = "PGUSER"
			description  = "Username to connect to PostgreSQL"
		)

		RootCmd.PersistentFlags().StringP(longName, shortName, defaultValue, description)
		viper.BindPFlag(key, RootCmd.PersistentFlags().Lookup(longName))
		viper.BindEnv(key, envVar)
		viper.SetDefault(key, defaultValue)
	}

	{
		const (
			key          = config.KeyPGPassword
			defaultValue = ""
			envVar       = "PGPASSWORD"
		)

		viper.BindEnv(key, envVar)
		viper.SetDefault(key, defaultValue)
	}

	{
		const (
			key          = config.KeyPProfEnable
			longName     = "enable-pprof"
			shortName    = ""
			defaultValue = true
			description  = "Enable the pprof endpoint interface"
		)

		RootCmd.PersistentFlags().BoolP(longName, shortName, defaultValue, description)
		viper.BindPFlag(key, RootCmd.PersistentFlags().Lookup(longName))
		viper.SetDefault(key, defaultValue)
	}

	{
		const (
			key          = config.KeyPProfPort
			longName     = "pprof-port"
			shortName    = ""
			defaultValue = 4242
			description  = "Specify the pprof port"
		)

		RootCmd.PersistentFlags().Uint16P(longName, shortName, defaultValue, description)
		viper.BindPFlag(key, RootCmd.PersistentFlags().Lookup(longName))
		viper.SetDefault(key, defaultValue)
	}
}

// initConfig reads in config file and ENV variables if set.
func initConfig() {
	viper.SetConfigName(buildtime.PROGNAME)

	if cfgFile != "" {
		// Use config file from the flag.
		viper.SetConfigFile(cfgFile)
	} else {
		d, err := os.Getwd()
		if err != nil {
			log.Warn().Err(err).Msg("unable to find the current working directory")
		} else {
			viper.AddConfigPath(d)
		}
	}

	// If a config file is found, read it in.
	if err := viper.ReadInConfig(); err != nil {
		log.Warn().Err(err).Msg("Unable to read config file")
	}
}
