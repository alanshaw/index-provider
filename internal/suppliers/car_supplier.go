package suppliers

import (
	"crypto/sha256"
	"encoding/base64"
	"io"
	"path/filepath"

	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	"github.com/ipld/go-car/v2"
	"github.com/multiformats/go-multicodec"
	"github.com/multiformats/go-multihash"
)

const carPathKeyPrefix = "car://"

var (
	_ CidIteratorSupplier = (*CarSupplier)(nil)
	_ io.Closer           = (*CarSupplier)(nil)
	_ io.Closer           = (*carCidIterator)(nil)
	_ CidIterator         = (*carCidIterator)(nil)
)

type CarSupplier struct {
	ds   datastore.Datastore
	opts []car.ReadOption
}

func NewCarSupplier(ds datastore.Datastore, opts ...car.ReadOption) *CarSupplier {
	return &CarSupplier{
		ds:   ds,
		opts: opts,
	}
}

// Put makes the CAR at given path suppliable by this supplier.
// The return CID can then be used via Supply to get an iterator over CIDs that belong to the CAR.
// The ID is generated based on the content of the CAR.
// When the CAR ID is already known, PutWithID should be used instead.
// This function accepts both CARv1 and CARv2 formats.
func (cs *CarSupplier) Put(path string) (cid.Cid, error) {
	// Clean path to CAR.
	path = filepath.Clean(path)

	// Generate a CID for the CAR at given path.
	id, err := generateID(path)
	if err != nil {
		return cid.Undef, err
	}

	return cs.PutWithID(id, path)
}

// PutWithID makes the CAR at given path suppliable by this supplier identified by the given ID.
// The return CID can then be used via Supply to get an iterator over CIDs that belong to the CAR.
// When the CAR ID is not known, Put should be used instead.
// This function accepts both CARv1 and CARv2 formats.
func (cs *CarSupplier) PutWithID(id cid.Cid, path string) (cid.Cid, error) {
	// Clean path to CAR.
	path = filepath.Clean(path)

	// Store mapping of CAR ID to path, used to instantiate CID iterator.
	carIdKey := toCarIdKey(id)
	if err := cs.ds.Put(carIdKey, []byte(path)); err != nil {
		return cid.Undef, err
	}

	// Store mapping of path to CAR ID, used to lookup the CAR by path when it is removed.
	if err := cs.ds.Put(toPathKey(path), id.Bytes()); err != nil {
		return cid.Undef, err
	}
	return id, nil
}

func toCarIdKey(id cid.Cid) datastore.Key {
	return datastore.NewKey(id.String())
}

// Remove removes the CAR at the given path from the list of suppliable CID iterators.
// If the CAR at given path is not known, this function will return an error.
// This function accepts both CARv1 and CARv2 formats.
func (cs *CarSupplier) Remove(path string) (cid.Cid, error) {
	// Clean path.
	path = filepath.Clean(path)

	// Find the CAR ID that corresponds to the given path
	pathKey := toPathKey(path)
	id, err := cs.getCarIDFromPathKey(pathKey)
	if err != nil {
		if err == datastore.ErrNotFound {
			err = ErrNotFound
		}
		return cid.Undef, err
	}

	// Delete mapping of CAR ID to path.
	carIdKey := toCarIdKey(id)
	if err := cs.ds.Delete(carIdKey); err != nil {
		// TODO improve error handling logic
		// we shouldn't typically get NotFound error here.
		// If we do then a put must have failed prematurely
		// See what we can do to opportunistically heal the datastore.
		return cid.Undef, err
	}

	// Delete mapping of path to CAR ID.
	if err := cs.ds.Delete(pathKey); err != nil {
		// TODO improve error handling logic
		// we shouldn't typically get NotFound error here.
		// If we do then a put must have failed prematurely
		// See what we can do to opportunistically heal the datastore.
		return cid.Undef, err
	}
	return id, nil
}

// Supply supplies an iterator over CIDs of the CAR file that corresponds to the given key.
// An error is returned if no CAR file is found for the key.
func (cs *CarSupplier) Supply(key cid.Cid) (CidIterator, error) {
	b, err := cs.ds.Get(toCarIdKey(key))
	if err != nil {
		if err == datastore.ErrNotFound {
			return nil, ErrNotFound
		}
		return nil, err
	}
	path := string(b)
	return newCarCidIterator(path, cs.opts...)
}

// Close permanently closes this supplier.
// After calling Close this supplier is no longer usable.
func (cs *CarSupplier) Close() error {
	return cs.ds.Close()
}

func (cs *CarSupplier) getCarIDFromPathKey(pathKey datastore.Key) (cid.Cid, error) {
	carIdBytes, err := cs.ds.Get(pathKey)
	if err != nil {
		return cid.Undef, err
	}
	_, c, err := cid.CidFromBytes(carIdBytes)
	return c, err
}

// generateID generates a unique ID for a CAR at a given path.
// The ID is in form of a CID, generated by hashing the list of all CIDs inside the CAR payload.
// This implies that different CARs that have the same CID list appearing in the same order will have the same ID, regardless of version.
// For example, CARv1 and wrapped CARv2 version of it will have the same CID list.
// This function accepts both CARv1 and CARv2 payloads
func generateID(path string, opts ...car.ReadOption) (cid.Cid, error) {
	// TODO investigate if there is a more efficient and version-agnostic way to generate CID for a CAR file.
	// HINT it will most likely be more efficient to generate the ID using the index of a CAR if it is an indexed CARv2
	// and fall back on current approach otherwise. Note, the CAR index has the multihashes of CIDs not full CIDs,
	// and that should be enough for the purposes of ID generation.

	// Instantiate iterator over CAR CIDs.
	cri, err := newCarCidIterator(path, opts...)
	if err != nil {
		return cid.Undef, err
	}
	defer cri.Close()
	// Instantiate a reader over the CID iterator to turn CIDs into bytes.
	// Note we use the multihash of CIDs instead of the entire CID.
	// TODO consider implementing an efficient multihash iterator for cars.
	reader := NewCidIteratorReadCloser(cri, func(cid cid.Cid) ([]byte, error) { return cid.Hash(), nil })

	// Generate multihash of CAR's CIDs.
	mh, err := multihash.SumStream(reader, multihash.SHA2_256, -1)
	if err != nil {
		return cid.Undef, err
	}
	// TODO Figure out what the codec should be.
	// HINT we could use the root CID codec or the first CID's codec.
	// Construct the ID for the CAR in form of a CID.
	return cid.NewCidV1(uint64(multicodec.DagCbor), mh), nil
}

func toPathKey(path string) datastore.Key {
	// Hash the path to get a fixed length string as key, regardless of how long the path is.
	pathHash := sha256.New().Sum([]byte(path))
	encPathHash := base64.StdEncoding.EncodeToString(pathHash)

	// Prefix the hash with some constant for namespacing and debug readability.
	return datastore.NewKey(carPathKeyPrefix + encPathHash)
}