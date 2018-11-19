package main

import (
	"flag"
	"os"

	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/promlog"
	"github.com/weaveworks/common/logging"
	"github.com/weaveworks/common/server"
	"github.com/weaveworks/cortex/pkg/util"

	"github.com/grafana/tempo/pkg/flagext"
	"github.com/grafana/tempo/pkg/promtail"
)

func main() {
	var (
		flagset         = flag.NewFlagSet("", flag.ExitOnError)
		configFile      = flagset.String("config.file", "promtail.yml", "The config file.")
		logLevel        = promlog.AllowedLevel{}
		serverConfig    server.Config
		clientConfig    promtail.ClientConfig
		positionsConfig promtail.PositionsConfig
	)
	flagext.Var(flagset, &logLevel, "log.level", "info", "")
	flagext.RegisterConfigs(flagset, &serverConfig, &clientConfig, &positionsConfig)
	flagset.Parse(os.Args[1:])

	logging.Setup(logLevel.String())
	util.InitLogger(logLevel)

	client, err := promtail.NewClient(clientConfig)
	if err != nil {
		level.Error(util.Logger).Log("msg", "Failed to create client", "error", err)
		return
	}
	defer client.Stop()

	positions, err := promtail.NewPositions(positionsConfig)
	if err != nil {
		level.Error(util.Logger).Log("msg", "Failed to read positions", "error", err)
		return
	}

	cfg, err := promtail.LoadConfig(*configFile)
	if err != nil {
		level.Error(util.Logger).Log("msg", "Failed to load config", "error", err)
		return
	}

	newTargetFunc := func(path string, labels model.LabelSet) (*promtail.Target, error) {
		return promtail.NewTarget(client, positions, path, labels)
	}
	tm, err := promtail.NewTargetManager(util.Logger, cfg.ScrapeConfig, newTargetFunc)
	if err != nil {
		level.Error(util.Logger).Log("msg", "Failed to make target manager", "error", err)
		return
	}
	defer tm.Stop()

	server, err := server.New(serverConfig)
	if err != nil {
		level.Error(util.Logger).Log("msg", "Error creating server", "error", err)
		return
	}

	defer server.Shutdown()
	server.Run()
}
