package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path"
	"time"

	httpfinderclient "github.com/filecoin-project/storetheindex/api/v0/finder/client/http"
	"github.com/filecoin-project/storetheindex/api/v0/finder/model"
	"github.com/ipld/go-car/v2"
	"github.com/ipld/go-car/v2/index"
	"github.com/multiformats/go-multicodec"
	"github.com/multiformats/go-multihash"
	"github.com/urfave/cli/v2"
)

var (
	carPath         string
	carIndexPath    string
	indexerAddr     string
	provId          string
	samplingProb    float64
	rngSeed         int64
	include         sampleSelector
	VerifyIngestCmd = &cli.Command{
		Name:  "verify-ingest",
		Usage: "Verifies ingestion of multihashes to an indexer node from a CAR file or a CARv2 Index",
		Description: `This command verifies whether a list of multihashes are ingested by an indexer node with the 
expected provider Peer ID. The multihashes to verify can be supplied from one of the following 
sources:
- Path to a CAR file (i.e. --from-car)
- Path to a CARv2 index file in iterable multihash format (i.e. --from-car-index)

The path to CAR files may point to any CAR version (CARv1 or CARv2). The list of multihashes are 
generated automatically from the CAR payload if no suitable index is not present. Note that the 
index is generated if either no index is present, or the existing index format or characteristics do
not match the desired values.

The path to CARv2 index file must point to an index in iterable multihash format, i.e. have 
'multicodec.CarMultihashIndexSorted'. See: https://github.com/ipld/go-car

In addition to the source, the user must also specify the address of indexer node and the expected 
provider's Peer ID.

By default, all of multihashes from source are used for verification. The user may specify a 
sampling probability to define the chance of each multihash being selected for verification. A
uniform random distribution is used to select whether a multihash should be used. The random number
generator is non-deterministic by default. However, a seed may be specified to make the selection
deterministic for debugging purposes.

Example usage:

* Verify ingestion from CAR file, selecting 50% of available multihashes using a deterministic 
random number generator, seeded with '1413':
	provider verify-ingest \
		--from-car my-dag.car \
		--to 192.168.2.100:3000 \
		--provider-id 12D3KooWE8yt84RVwW3sFcd6WMjbUdWrZer2YtT4dmtj3dHdahSZ \
		--sampling-prob 0.5 \
		--rng-seed 1413

* Verify ingestion from CARv2 index file using all available multihashes, i.e. unspecified 
sampling probability defaults to 1.0 (100%):
	provider verify-ingest \
		--from-car my-idx.idx \
		--to 192.168.2.100:3000 \
		--provider-id 12D3KooWE8yt84RVwW3sFcd6WMjbUdWrZer2YtT4dmtj3dHdahSZ

The output respectively prints:
- The number of multihashes the tool failed to verify, e.g. due to communication error.
- The number of multihashes not indexed by the indexer.
- The number of multihashes known by the indexer but not associated to the given provider Peer ID.
- The number of multihashes known with expected provider Peer ID.
- And finally, total number of multihashes verified.

A verification is considered as passed when the total number of multihashes checked matches the 
number of multihashes that are indexed with expected provider Peer ID.

Example output:

* Passed verification:
	Verification result:
	  # failed to verify:                   0
	  # unindexed:                          0
	  # indexed with another provider ID:   0
	  # indexed with expected provider ID:  1049
	--------------------------------------------
	total Multihashes checked:              1049
	
	sampling probability:                   1.00
	RNG seed:                               0
	
	🎉 Passed verification check.

* Failed verification:
	Verification result:
	  # failed to verify:                   0
	  # unindexed:                          20
	  # indexed with another provider ID:   0
	  # indexed with expected provider ID:  0
	--------------------------------------------
	total Multihashes checked:              20
	
	sampling probability:                   0.50
	RNG seed:                               42
	
	❌ Failed verification check.
`,
		Aliases: []string{"vi"},
		Flags: []cli.Flag{
			&cli.PathFlag{
				Name:        "from-car",
				Usage:       "Path to the CAR file from which to extract the list of multihash for verification.",
				Aliases:     []string{"fc"},
				Destination: &carPath,
			},
			&cli.PathFlag{
				Name:        "from-car-index",
				Usage:       "Path to the CAR index file from which to extract the list of multihash for verification.",
				Aliases:     []string{"fci"},
				Destination: &carIndexPath,
			},

			&cli.StringFlag{
				Name:        "to",
				Aliases:     []string{"i"},
				Usage:       "The host:port of the indexer node to which to verify ingestion.",
				Destination: &indexerAddr,
				Required:    true,
			},
			&cli.StringFlag{
				Name:        "provider-id",
				Aliases:     []string{"pid"},
				Usage:       "The peer ID of the provider that should be associated to multihashes.",
				Required:    true,
				Destination: &provId,
			},
			&cli.Float64Flag{
				Name:        "sampling-prob",
				Aliases:     []string{"sp"},
				Usage:       "The uniform random probability of selecting a multihash for verification specified as a value between 0.0 and 1.0.",
				DefaultText: "'1.0' i.e. 100% of multihashes will be checked for verification.",
				Value:       1.0,
				Destination: &samplingProb,
			},
			&cli.Int64Flag{
				Name:    "rng-seed",
				Aliases: []string{"rs"},
				Usage: "The seed to use for the random number generator that selects verification samples. " +
					"This flag has no impact if sampling probability is set to 1.0.",
				DefaultText: "Non-deterministic.",
				Destination: &rngSeed,
			},
		},
		Before: beforeVerifyIngest,
		Action: doVerifyIngest,
	}
)

type sampleSelector func() bool

func beforeVerifyIngest(cctx *cli.Context) error {
	if samplingProb <= 0 || samplingProb > 1 {
		showVerifyIngestHelp(cctx)
		return cli.Exit("Sampling probability must be larger than 0.0 and smaller or equal to 1.0.", 1)
	}

	if samplingProb == 1 {
		include = func() bool {
			return true
		}
	} else {
		if rngSeed == 0 {
			rngSeed = time.Now().UnixNano()
		}
		rng := rand.New(rand.NewSource(rngSeed))
		include = func() bool {
			return rng.Float64() <= samplingProb
		}
	}
	return nil
}

func doVerifyIngest(cctx *cli.Context) error {
	if carPath != "" {
		if carIndexPath != "" {
			return errVerifyIngestMultipleSources(cctx)
		}
		carPath = path.Clean(carPath)
		return doVerifyIngestFromCar(cctx)
	}

	if carIndexPath == "" {
		showVerifyIngestHelp(cctx)
		return cli.Exit("Exactly one multihash source must be specified.", 1)
	}
	carIndexPath = path.Clean(carIndexPath)
	return doVerifyIngestFromCarIndex()
}

func doVerifyIngestFromCar(_ *cli.Context) error {
	idx, err := getOrGenerateCarIndex()
	if err != nil {
		return err
	}

	finder, err := httpfinderclient.New(indexerAddr)
	if err != nil {
		return err
	}

	result, err := verifyIngestFromCarIterableIndex(finder, idx)
	if err != nil {
		return err
	}

	return result.printAndExit()
}

func getOrGenerateCarIndex() (index.IterableIndex, error) {
	cr, err := car.OpenReader(carPath)
	if err != nil {
		return nil, err
	}
	idxReader := cr.IndexReader()
	if err != nil {
		return nil, err
	}

	if idxReader == nil {
		return generateIterableIndex(cr)
	}

	idx, err := index.ReadFrom(idxReader)
	if err != nil {
		return nil, err
	}
	if idx.Codec() != multicodec.CarMultihashIndexSorted {
		// Index doesn't contain full multihashes; generate it.
		return generateIterableIndex(cr)
	}
	return idx.(index.IterableIndex), nil
}

func generateIterableIndex(cr *car.Reader) (index.IterableIndex, error) {
	idx := index.NewMultihashSorted()
	if err := car.LoadIndex(idx, cr.DataReader()); err != nil {
		return nil, err
	}
	return idx, nil
}

func doVerifyIngestFromCarIndex() error {
	idxFile, err := os.Open(carIndexPath)
	if err != nil {
		return err
	}
	idx, err := index.ReadFrom(idxFile)
	if err != nil {
		return err
	}

	iterIdx, ok := idx.(index.IterableIndex)
	if !ok {
		return errInvalidCarIndexFormat()
	}

	finder, err := httpfinderclient.New(indexerAddr)
	if err != nil {
		return err
	}

	result, err := verifyIngestFromCarIterableIndex(finder, iterIdx)
	if err != nil {
		return err
	}

	return result.printAndExit()
}

func errInvalidCarIndexFormat() cli.ExitCoder {
	return cli.Exit("CAR index must be in iterable multihash format; see: multicodec.CarMultihashIndexSorted", 1)
}

func errVerifyIngestMultipleSources(cctx *cli.Context) error {
	showVerifyIngestHelp(cctx)
	return cli.Exit("Multiple multihash sources are specified. Only a single source at a time is supported.", 1)
}

func showVerifyIngestHelp(cctx *cli.Context) {
	// Ignore error since it only occues if usage is not found for command.
	_ = cli.ShowCommandHelp(cctx, "verify-ingest")
}

type verifyIngestResult struct {
	total             int
	providerMissmatch int
	present           int
	absent            int
	err               int
}

func (r *verifyIngestResult) passedVerification() bool {
	return r.present == r.total
}

func (r *verifyIngestResult) printAndExit() error {
	fmt.Println()
	fmt.Println("Verification result:")
	fmt.Printf("  # failed to verify:                   %d\n", r.err)
	fmt.Printf("  # unindexed:                          %d\n", r.absent)
	fmt.Printf("  # indexed with another provider ID:   %d\n", r.providerMissmatch)
	fmt.Printf("  # indexed with expected provider ID:  %d\n", r.present)
	fmt.Println("--------------------------------------------")
	fmt.Printf("total Multihashes checked:              %d\n", r.total)
	fmt.Println()
	fmt.Printf("sampling probability:                   %.2f\n", samplingProb)
	fmt.Printf("RNG seed:                               %d\n", rngSeed)
	fmt.Println()
	if r.passedVerification() {
		return cli.Exit("🎉 Passed verification check.", 0)
	}
	return cli.Exit("❌ Failed verification check.", 1)
}

func verifyIngestFromCarIterableIndex(finder *httpfinderclient.Client, idx index.IterableIndex) (*verifyIngestResult, error) {
	result := &verifyIngestResult{}
	var mhs []multihash.Multihash

	if err := idx.ForEach(func(mh multihash.Multihash, _ uint64) error {
		if include() {
			mhs = append(mhs, mh)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	mhsCount := len(mhs)
	result.total = mhsCount
	response, err := finder.FindBatch(context.Background(), mhs)
	if err != nil {
		// Set number multihashes failed to verify instead of returning error since at this point
		// the number of multihashes is known.
		result.err = mhsCount
		return result, nil
	}

	if len(response.MultihashResults) == 0 {
		result.absent = mhsCount
		return result, nil
	}

	mhLookup := make(map[string]model.MultihashResult)
	for _, mr := range response.MultihashResults {
		mhLookup[mr.Multihash.String()] = mr
	}

	for _, mh := range mhs {
		mr, ok := mhLookup[mh.String()]
		if !ok {
			result.absent++
			continue
		}

		if len(mr.ProviderResults) == 0 {
			result.absent++
			continue
		}

		var matchedProvider bool
		for _, p := range mr.ProviderResults {
			id := p.Provider.ID.String()
			if id == provId {
				result.present++
				matchedProvider = true
				break
			}
		}
		if !matchedProvider {
			result.providerMissmatch++
		}
	}
	return result, nil
}