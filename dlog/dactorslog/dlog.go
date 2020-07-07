package dactorslog

import (
	"github.com/filecoin-project/specs-actors/util"
	"go.uber.org/zap"
)

var L *zap.Logger

func init() {
	L = util.GetXDebugLog("actors")
}
