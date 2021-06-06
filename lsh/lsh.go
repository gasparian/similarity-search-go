package lsh

import (
	"container/heap"
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

// Neighbor represent neighbor vector with distance to the query vector
type Neighbor struct {
	Record
	Dist float64
}

type FloatMinHeap []Neighbor

func (h FloatMinHeap) Len() int {
	return len(h)
}

func (h FloatMinHeap) Less(i, j int) bool {
	return h[i].Dist < h[j].Dist
}

func (h FloatMinHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *FloatMinHeap) Push(x interface{}) {
	*h = append(*h, x.(Neighbor))
}

func (h *FloatMinHeap) Pop() interface{} {
	tailIndex := h.Len() - 1
	tail := (*h)[tailIndex]
	*h = (*h)[:tailIndex]
	return tail
}

// Metric holds implementation of needed distance metric
type Metric interface {
	GetDist(l, r []float64) float64
}

// Indexer holds implementation of NN search index
type Indexer interface {
	Train(records []Record) error
	Search(query []float64, maxNN int, distanceThrsh float64) ([]Neighbor, error)
}

// IndexConfig ...
type IndexConfig struct {
	mx            *sync.RWMutex
	BatchSize     int
	Bias          []float64
	Std           []float64
	MaxCandidates int
}

// Config holds all needed constants for creating the Hasher instance
type Config struct {
	IndexConfig
	HasherConfig
}

// LSHIndex holds buckets with vectors and hasher instance
type LSHIndex struct {
	config         IndexConfig
	index          store.Store
	hasher         *Hasher
	scaler         *StandartScaler
	distanceMetric Metric
}

// New creates new instance of hasher and index, where generated hashes will be stored
func NewLsh(config Config, store store.Store, metric Metric) (*LSHIndex, error) {
	if config.Std == nil || len(config.Std) == 0 || blas64.Asum(NewVec(config.Std)) < tol {
		config.HasherConfig.isCrossOrigin = true
	}
	hasher := NewHasher(
		config.HasherConfig,
	)
	err := hasher.generate()
	if err != nil {
		return nil, err
	}
	config.IndexConfig.mx = new(sync.RWMutex)
	return &LSHIndex{
		config:         config.IndexConfig,
		hasher:         hasher,
		index:          store,
		distanceMetric: metric,
		scaler:         NewStandartScaler(config.Bias, config.Std, config.HasherConfig.Dims),
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
	lsh.config.mx.RUnlock()

	wg := sync.WaitGroup{}
	for i := 0; i < len(records); i += batchSize {
		wg.Add(1)
		end := i + batchSize
		if end > len(records) {
			end = len(records)
		}
		go func(batch []Record, wg *sync.WaitGroup) {
			defer wg.Done()
			for _, rec := range batch {
				scaled := lsh.scaler.Scale(rec.Vec)
				hashes := lsh.hasher.getHashes(scaled)
				lsh.index.SetVector(rec.ID, rec.Vec)
				for perm, hash := range hashes {
					bucketName := getBucketName(perm, hash)
					lsh.index.SetHash(bucketName, rec.ID)
				}
			}
		}(records[i:end], &wg)
	}
	wg.Wait()
	return nil
}

// Search returns NNs for the query point
func (lsh *LSHIndex) Search(query []float64, maxNN int, distanceThrsh float64) ([]Neighbor, error) {
	lsh.config.mx.RLock()
	maxCandidates := lsh.config.MaxCandidates
	lsh.config.mx.RUnlock()
	scaledQuery := lsh.scaler.Scale(query)
	hashes := lsh.hasher.getHashes(scaledQuery)

	closestSet := make(map[string]bool)
	minHeap := new(FloatMinHeap)
	for perm, hash := range hashes {
		if minHeap.Len() >= maxCandidates {
			break
		}
		bucketName := getBucketName(perm, hash)
		iter, err := lsh.index.GetHashIterator(bucketName)
		if err != nil {
			continue // NOTE: it's normal when we couldn't find bucket for the query point
		}
		for {
			if minHeap.Len() >= maxCandidates {
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
			if dist <= distanceThrsh {
				closestSet[id] = true
				heap.Push(
					minHeap,
					Neighbor{
						Record: Record{ID: id, Vec: vec},
						Dist:   dist,
					},
				)
			}
		}
	}
	closest := make([]Neighbor, 0)
	for i := 0; i < maxNN && minHeap.Len() > 0; i++ {
		closest = append(closest, heap.Pop(minHeap).(Neighbor))
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
