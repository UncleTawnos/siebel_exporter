// Package log provides a thin, logrus-backed logging facade.
//
// It replicates the small subset of the now-removed
// github.com/prometheus/common/log API that this project relies on, so the
// rest of the codebase can keep using the familiar Debugln/Errorf/... helpers
// while depending only on maintained libraries.
package log

import (
	stdlog "log"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	"github.com/sirupsen/logrus"
)

func init() {
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})
	logrus.SetLevel(logrus.InfoLevel)
}

// AddFlags registers the --log.level and --log.format flags on the given
// kingpin application. The flags are applied during flag parsing.
func AddFlags(a *kingpin.Application) {
	a.Flag("log.level", "Only log messages with the given severity or above. One of: [debug, info, warn, error].").
		Default("info").SetValue(&levelFlag{})
	a.Flag("log.format", "Output format of log messages. One of: [text, json].").
		Default("text").SetValue(&formatFlag{})
}

type levelFlag struct{}

func (levelFlag) String() string { return logrus.GetLevel().String() }

func (levelFlag) Set(s string) error {
	lvl, err := logrus.ParseLevel(strings.TrimSpace(s))
	if err != nil {
		return err
	}
	logrus.SetLevel(lvl)
	return nil
}

type formatFlag struct{}

func (formatFlag) String() string { return "text" }

func (formatFlag) Set(s string) error {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "json":
		logrus.SetFormatter(&logrus.JSONFormatter{})
	case "text", "logfmt", "":
		logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
	default:
		// Keep the previously configured formatter for unknown values.
	}
	return nil
}

// Logging helpers mirroring the prometheus/common/log surface used here.

func Debug(args ...interface{})                 { logrus.Debug(args...) }
func Debugln(args ...interface{})               { logrus.Debugln(args...) }
func Debugf(format string, args ...interface{}) { logrus.Debugf(format, args...) }

func Info(args ...interface{})                 { logrus.Info(args...) }
func Infoln(args ...interface{})               { logrus.Infoln(args...) }
func Infof(format string, args ...interface{}) { logrus.Infof(format, args...) }

func Warn(args ...interface{})                 { logrus.Warn(args...) }
func Warnln(args ...interface{})               { logrus.Warnln(args...) }
func Warnf(format string, args ...interface{}) { logrus.Warnf(format, args...) }

func Error(args ...interface{})                 { logrus.Error(args...) }
func Errorln(args ...interface{})               { logrus.Errorln(args...) }
func Errorf(format string, args ...interface{}) { logrus.Errorf(format, args...) }

// NewErrorLogger returns a *log.Logger that forwards everything it receives to
// logrus at error level. It is intended for wiring into standard-library
// consumers such as http.Server.ErrorLog and promhttp.HandlerOpts.ErrorLog.
func NewErrorLogger() *stdlog.Logger {
	return stdlog.New(errorWriter{}, "", 0)
}

type errorWriter struct{}

func (errorWriter) Write(p []byte) (int, error) {
	logrus.Error(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
