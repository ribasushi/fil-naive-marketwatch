package main //nolint:revive

import (
	"context"
	"fmt"
	"os"

	filbuiltin "github.com/filecoin-project/go-state-types/builtin"
	logging "github.com/ipfs/go-log/v2"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/ribasushi/go-toolbox-interplanetary/fil"
	"github.com/ribasushi/go-toolbox/cmn"
	"github.com/ribasushi/go-toolbox/ufcli"
)

func main() {
	appName := "naivewatch"

	cmdName := appName + "-cron"
	log := logging.Logger(fmt.Sprintf("%s(%d)", cmdName, os.Getpid()))
	logging.SetLogLevel("*", "INFO") //nolint:errcheck

	home, err := os.UserHomeDir()
	if err != nil {
		log.Error(cmn.WrErr(err))
		os.Exit(1)
	}

	(&ufcli.UFcli{

		Logger: log,

		TOMLPath: fmt.Sprintf("%s/%s.toml", home, appName),

		AppConfig: ufcli.App{
			Name:  cmdName,
			Usage: "Misc background processes for " + appName,
			Commands: []*ufcli.Command{
				pollProviders,
				trackDeals,
			},
			Flags: []ufcli.Flag{
				ufcli.ConfStringFlag(&ufcli.StringFlag{
					Name:  "lotus-api",
					Value: "http://localhost:1234",
				}),
				&ufcli.UintFlag{
					Name:  "lotus-lookback-epochs",
					Value: filDefaultLookback,
					DefaultText: fmt.Sprintf("%d epochs / %ds",
						filDefaultLookback,
						filbuiltin.EpochDurationSeconds*filDefaultLookback,
					),
					Destination: &lotusLookbackEpochs,
				},
				ufcli.ConfStringFlag(&ufcli.StringFlag{
					Name:  "pg-connstring",
					Value: "postgres:///dbname?user=username&password=&host=/var/run/postgresql",
				}),
			},
		},

		GlobalInit: func(cctx *ufcli.Context, uf *ufcli.UFcli) (func() error, error) {
			gctx := GlobalContext{
				Logger: uf.Logger,
			}

			// can do it here, since now we know the config
			dbConnCfg, err := pgxpool.ParseConfig(cctx.String("pg-connstring"))
			if err != nil {
				return nil, cmn.WrErr(err)
			}
			dbConnCfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
				return nil
			}
			gctx.Db, err = pgxpool.ConnectConfig(cctx.Context, dbConnCfg)
			if err != nil {
				return nil, cmn.WrErr(err)
			}

			lAPI, apiCloser, err := fil.LotusAPIClientV0(
				cctx.Context,
				cctx.String("lotus-api"),
				300,
				"",
			)
			if err != nil {
				return nil, cmn.WrErr(err)
			}
			gctx.LotusAPI = lAPI

			cctx.Context = context.WithValue(cctx.Context, ck, gctx)
			return func() error {
				apiCloser()
				gctx.Db.Close()
				return nil
			}, nil
		},
	}).RunAndExit(context.Background())
}
