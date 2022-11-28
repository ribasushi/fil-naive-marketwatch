package cmn

//nolint:revive
const (
	AppName      = "naivewatch"
	PromInstance = "dataprogs_naivewatch"

	FilGenesisUnix      = 1598306400
	FilDefaultLookback  = 10
	ApiMaxTipsetsBehind = 3 // keep in mind that a nul tipset is indistinguishable from loss of sync - do not set too low
)
