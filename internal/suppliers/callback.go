package suppliers

import (
	"io"

	"github.com/filecoin-project/indexer-reference-provider/core"
	"github.com/ipfs/go-cid"
)

// ToCidCallback converts the given cidIter to core.CidCallback.
func ToCidCallback(cidIterSup CidIteratorSupplier) core.CidCallback {
	return func(key core.LookupKey) (<-chan cid.Cid, <-chan error) {
		ci, err := cidIterSup.Supply(key)
		if err != nil {
			errChan := make(chan error, 1)
			defer close(errChan)
			errChan <- err
			return nil, errChan
		}
		return toChan(ci)
	}
}

func toChan(ci CidIterator) (<-chan cid.Cid, <-chan error) {
	cidChan := make(chan cid.Cid, 1)
	errChan := make(chan error, 1)
	go func() {
		defer func() {
			close(cidChan)
			close(errChan)
		}()
		for {
			c, err := ci.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				errChan <- err
			}
			cidChan <- c
		}
	}()
	return cidChan, errChan
}