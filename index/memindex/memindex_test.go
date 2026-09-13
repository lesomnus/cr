package memindex_test

import (
	"testing"
	"time"

	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/index/indextest"
	"github.com/lesomnus/cr/index/memindex"
)

func TestIndex(t *testing.T) {
	indextest.Run(t, func(t *testing.T, wait time.Duration) index.Index {
		ix := memindex.New()
		ix.Wait = wait
		return ix
	})
}
