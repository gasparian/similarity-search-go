package lsh

import (
	"errors"
	"github.com/gasparian/lsh-search-go/store"
	"gonum.org/v1/gonum/blas/blas64"
	"sync"
)

var (
	DistanceErr = errors.New("Distance can't be calculated")
)

// Record holds vector and it's unique identifier generated by `user`
type Record struct {
	ID  string
	Vec []float64
}

// Metric holds implementation of needed distance metric
type Metric interface {
	GetDist(l, r []float64) float64
}

// Indexer holds implementation of NN search index
type Indexer interface {
	Train(records []Record) error
	Search(query []float64) ([]Record, error)
}

// LshConfig ...
type LshConfig struct {
	mx            *sync.RWMutex
	DistanceThrsh float64
	MaxNN         int
	BatchSize     int
	MeanVec       []float64

	dims    int
	meanVec blas64.Vector
}

// Config holds all needed constants for creating the Hasher instance
type Config struct {
	LshConfig
	HasherConfig
}

// LSHIndex holds buckets with vectors and hasher instance
type LSHIndex struct {
	config         LshConfig
	index          store.Store
	hasher         *Hasher
	distanceMetric Metric
}

func checkConvertVec(inp []float64, dims int) blas64.Vector {
	meanVecInternal := NewVec(make([]float64, dims))
	if inp != nil && len(inp) == dims {
		meanVecInternal.Data = inp
		copy(meanVecInternal.Data, inp)
	}
	return meanVecInternal
}

// New creates new instance of hasher and index, where generated hashes will be stored
func NewLsh(config Config, store store.Store, metric Metric) (*LSHIndex, error) {
	hasher := NewHasher(
		config.HasherConfig,
	)
	err := hasher.generate()
	if err != nil {
		return nil, err
	}
	config.LshConfig.mx = new(sync.RWMutex)
	config.LshConfig.dims = config.HasherConfig.Dims
	config.LshConfig.meanVec = checkConvertVec(config.MeanVec, config.LshConfig.dims)
	return &LSHIndex{
		config:         config.LshConfig,
		hasher:         hasher,
		index:          store,
		distanceMetric: metric,
	}, nil
}

// Train fills new search index with vectors
func (lsh *LSHIndex) Train(records []Record) error {
	err := lsh.index.Clear()
	if err != nil {
		return err
	}
	lsh.config.mx.RLock()
	batchSize := lsh.config.BatchSize
	meanVec := lsh.config.meanVec
	lsh.config.mx.RUnlock()

	wg := sync.WaitGroup{}
	wg.Add(len(records)/batchSize + len(records)%batchSize)
	for i := 0; i < len(records); i += batchSize {
		end := i + batchSize
		if end > len(records) {
			end = len(records)
		}
		go func(batch []Record, meanVec blas64.Vector, wg *sync.WaitGroup) {
			defer wg.Done()
			shifted := NewVec(make([]float64, meanVec.N))
			for _, rec := range batch {
				copy(shifted.Data, rec.Vec)
				blas64.Axpy(-1.0, meanVec, shifted)
				hashes := lsh.hasher.getHashes(shifted)
				lsh.index.SetVector(rec.ID, rec.Vec)
				for perm, hash := range hashes {
					lsh.index.SetHash(perm, hash, rec.ID)
				}
			}
		}(records[i:end], meanVec, &wg)
	}
	wg.Wait()
	return nil
}

// Search returns NNs for the query point
func (lsh *LSHIndex) Search(query []float64) ([]Record, error) {
	lsh.config.mx.RLock()
	config := lsh.config
	shiftedQuery := NewVec(make([]float64, len(query)))
	copy(shiftedQuery.Data, query)
	blas64.Axpy(-1.0, config.meanVec, shiftedQuery)
	lsh.config.mx.RUnlock()
	hashes := lsh.hasher.getHashes(shiftedQuery)

	closestSet := make(map[string]bool)
	closest := make([]Record, 0)
	for perm, hash := range hashes {
		if len(closest) >= config.MaxNN {
			break
		}
		iter, err := lsh.index.GetHashIterator(perm, hash)
		if err != nil {
			continue // NOTE: it's normal when we couldn't find bucket for the query point
		}
		for {
			if len(closest) >= config.MaxNN {
				break
			}
			id, opened := iter.Next()
			if !opened {
				break
			}

			if closestSet[id] {
				continue
			}
			vec, err := lsh.index.GetVector(id)
			if err != nil {
				return nil, err
			}
			dist := lsh.distanceMetric.GetDist(vec, query)
			if dist <= config.DistanceThrsh {
				closestSet[id] = true
				closest = append(closest, Record{ID: id, Vec: vec})
			}
		}
	}
	return closest, nil
}

// DumpHasher serializes hasher
func (lsh *LSHIndex) DumpHasher() ([]byte, error) {
	return lsh.hasher.dump()
}

// LoadHasher fills hasher from byte array
func (lsh *LSHIndex) LoadHasher(inp []byte) error {
	return lsh.hasher.load(inp)
}
