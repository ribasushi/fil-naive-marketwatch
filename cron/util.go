package main

import (
	"context"

	lotusapi "github.com/filecoin-project/lotus/api"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/ribasushi/go-toolbox/ufcli"
)

type GlobalContext struct { //nolint:revive
	Db       *pgxpool.Pool
	LotusAPI *lotusapi.FullNodeStruct
	Logger   ufcli.Logger
}

type ctxKey string

var ck = ctxKey("🤢")

func GetGlobalCtx(ctx context.Context) GlobalContext { //nolint:revive
	return ctx.Value(ck).(GlobalContext)
}

func UnpackCtx(ctx context.Context) ( //nolint:revive
	origCtx context.Context,
	logger ufcli.Logger,
	mainDB *pgxpool.Pool,
	lotusAPI *lotusapi.FullNodeStruct,
) {
	gctx := GetGlobalCtx(ctx)
	return ctx, gctx.Logger, gctx.Db, gctx.LotusAPI
}
