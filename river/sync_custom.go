package river

import (
	"github.com/go-mysql-org/go-mysql-elasticsearch/elastic"
	"github.com/go-mysql-org/go-mysql/canal"
	"github.com/juju/errors"
)

type customEventHandler struct {
	eventHandler
}

// TODO add custom logic
func (h *customEventHandler) OnRow(e *canal.RowsEvent) error {
	var reqs []*elastic.BulkRequest
	var err error
	switch e.Action {
	case canal.InsertAction:
		reqs, err = h.r.makeInsertRequest(nil, e.Rows)
	case canal.DeleteAction:
		reqs, err = h.r.makeDeleteRequest(nil, e.Rows)
	case canal.UpdateAction:
		reqs, err = h.r.makeUpdateRequest(nil, e.Rows)
	default:
		err = errors.Errorf("invalid rows action %s", e.Action)
	}

	if err != nil {
		h.r.cancel()
		return errors.Errorf("make %s ES request err %v, close sync", e.Action, err)
	}

	h.r.syncCh <- reqs

	return h.r.ctx.Err()
}
