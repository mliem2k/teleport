package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Request/Response models
type Truck struct {
	ID            string `json:"id"`
	MaxWeightLbs  int64  `json:"max_weight_lbs"`
	MaxVolumeCuft int64  `json:"max_volume_cuft"`
}

type Order struct {
	ID            string `json:"id"`
	PayoutCents   int64  `json:"payout_cents"`
	WeightLbs     int64  `json:"weight_lbs"`
	VolumeCuft    int64  `json:"volume_cuft"`
	Origin        string `json:"origin"`
	Destination   string `json:"destination"`
	PickupDate    string `json:"pickup_date"`
	DeliveryDate  string `json:"delivery_date"`
	IsHazmat      bool   `json:"is_hazmat"`
}

type OptimizeRequest struct {
	Truck   Truck   `json:"truck"`
	Orders  []Order `json:"orders"`
}

type OptimizeResponse struct {
	TruckID                 string   `json:"truck_id"`
	SelectedOrderIDs        []string `json:"selected_order_ids"`
	TotalPayoutCents        int64    `json:"total_payout_cents"`
	TotalWeightLbs          int64    `json:"total_weight_lbs"`
	TotalVolumeCuft         int64    `json:"total_volume_cuft"`
	UtilizationWeightPercent float64 `json:"utilization_weight_percent"`
	UtilizationVolumePercent float64 `json:"utilization_volume_percent"`
}

type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// Cache entry
type cacheEntry struct {
	response   *OptimizeResponse
	expiration time.Time
}

// LRU cache for optimization results
type responseCache struct {
	mu    sync.RWMutex
	store map[string]*cacheEntry
	// LRU tracking
	keys []string
	maxSize int
}

// Global cache instance
var globalCache = newResponseCache(1000) // Cache up to 1000 responses

func newResponseCache(maxSize int) *responseCache {
	return &responseCache{
		store:   make(map[string]*cacheEntry),
		keys:    make([]string, 0, maxSize),
		maxSize: maxSize,
	}
}

// get retrieves a cached response if it exists and hasn't expired
func (c *responseCache) get(key string) (*OptimizeResponse, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, exists := c.store[key]
	if !exists {
		return nil, false
	}
	if time.Now().After(entry.expiration) {
		return nil, false
	}
	return entry.response, true
}

// put stores a response in the cache with TTL
func (c *responseCache) put(key string, response *OptimizeResponse, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Remove expired entries and make room if needed
	if len(c.keys) >= c.maxSize {
		// Simple FIFO eviction (could be upgraded to true LRU)
		delete(c.store, c.keys[0])
		c.keys = c.keys[1:]
	}

	c.store[key] = &cacheEntry{
		response:   response,
		expiration: time.Now().Add(ttl),
	}
	c.keys = append(c.keys, key)
}

// cacheKey generates a hash key from the request
func cacheKey(req *OptimizeRequest) (string, error) {
	// Create a deterministic representation of the request
	data, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

// Optimizer holds the optimization state
type Optimizer struct {
	truck    Truck
	orders   []Order
	n        int
	maxMask  int
	// Pre-computed totals for each subset
	weight   []int64
	volume   []int64
	payout   []int64
	valid    []bool
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/api/v1/load-optimizer/optimize", optimizeHandler)

	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
		IdleTimeout:  10 * time.Second,
	}

	log.Println("Starting server on :8080")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
}

func optimizeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check content length (max 1MB)
	if r.ContentLength > 1<<20 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		json.NewEncoder(w).Encode(ErrorResponse{Error: "payload too large", Message: "payload too large"})
		return
	}

	var req OptimizeRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		msg := "invalid JSON: " + err.Error()
		json.NewEncoder(w).Encode(ErrorResponse{Error: msg, Message: msg})
		return
	}

	// Validate request
	if err := validateRequest(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		msg := err.Error()
		json.NewEncoder(w).Encode(ErrorResponse{Error: msg, Message: msg})
		return
	}

	// Check cache first
	key, err := cacheKey(&req)
	if err == nil {
		if cached, found := globalCache.get(key); found {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(cached)
			return
		}
	}

	// Solve optimization problem
	response := solve(&req)

	// Store in cache (5 minute TTL)
	if err == nil {
		globalCache.put(key, response, 5*time.Minute)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "MISS")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

func validateRequest(req *OptimizeRequest) error {
	if req.Truck.ID == "" {
		return fmt.Errorf("truck.id is required")
	}
	if req.Truck.MaxWeightLbs <= 0 {
		return fmt.Errorf("truck.max_weight_lbs must be positive")
	}
	if req.Truck.MaxVolumeCuft <= 0 {
		return fmt.Errorf("truck.max_volume_cuft must be positive")
	}
	if len(req.Orders) > 22 {
		return fmt.Errorf("too many orders (max 22)")
	}
	for i, o := range req.Orders {
		if o.ID == "" {
			return fmt.Errorf("orders[%d].id is required", i)
		}
		if o.PayoutCents < 0 {
			return fmt.Errorf("orders[%d].payout_cents must be non-negative", i)
		}
		if o.WeightLbs < 0 {
			return fmt.Errorf("orders[%d].weight_lbs must be non-negative", i)
		}
		if o.VolumeCuft < 0 {
			return fmt.Errorf("orders[%d].volume_cuft must be non-negative", i)
		}
		if o.Origin == "" {
			return fmt.Errorf("orders[%d].origin is required", i)
		}
		if o.Destination == "" {
			return fmt.Errorf("orders[%d].destination is required", i)
		}
		if o.PickupDate == "" {
			return fmt.Errorf("orders[%d].pickup_date is required", i)
		}
		if o.DeliveryDate == "" {
			return fmt.Errorf("orders[%d].delivery_date is required", i)
		}
		// Validate pickup_date <= delivery_date
		pickup, err := time.Parse("2006-01-02", o.PickupDate)
		if err != nil {
			return fmt.Errorf("orders[%d].pickup_date has invalid format (expected YYYY-MM-DD): %s", i, o.PickupDate)
		}
		delivery, err := time.Parse("2006-01-02", o.DeliveryDate)
		if err != nil {
			return fmt.Errorf("orders[%d].delivery_date has invalid format (expected YYYY-MM-DD): %s", i, o.DeliveryDate)
		}
		if pickup.After(delivery) {
			return fmt.Errorf("orders[%d].pickup_date must be on or before delivery_date", i)
		}
	}
	return nil
}

// solve finds the optimal combination of orders using DP with bitmask
func solve(req *OptimizeRequest) *OptimizeResponse {
	opt := NewOptimizer(req.Truck, req.Orders)
	bestMask := opt.FindOptimal()

	return opt.BuildResponse(bestMask)
}

// NewOptimizer creates a new optimizer instance
func NewOptimizer(truck Truck, orders []Order) *Optimizer {
	n := len(orders)
	maxMask := 1 << n
	opt := &Optimizer{
		truck:   truck,
		orders:  orders,
		n:       n,
		maxMask: maxMask,
		weight:  make([]int64, maxMask),
		volume:  make([]int64, maxMask),
		payout:  make([]int64, maxMask),
		valid:   make([]bool, maxMask),
	}

	// Pre-compute totals for each subset using DP
	opt.precompute()

	return opt
}

// precompute calculates weight, volume, payout and validity for all subsets
// Uses subset DP: dp[mask] = dp[mask without LSB] + order[LSB index]
// Applies pruning: subsets exceeding truck capacity are marked invalid immediately
func (o *Optimizer) precompute() {
	// Empty set
	o.valid[0] = true
	o.weight[0] = 0
	o.volume[0] = 0
	o.payout[0] = 0

	maxWeight := o.truck.MaxWeightLbs
	maxVolume := o.truck.MaxVolumeCuft

	// For each non-empty subset
	for mask := 1; mask < o.maxMask; mask++ {
		// Get lowest set bit
		lsb := mask & -mask
		i := bitPosition(lsb)
		prev := mask ^ lsb

		o.weight[mask] = o.weight[prev] + o.orders[i].WeightLbs
		o.volume[mask] = o.volume[prev] + o.orders[i].VolumeCuft
		o.payout[mask] = o.payout[prev] + o.orders[i].PayoutCents

		// Pruning: check capacity constraints first (fast check)
		if o.weight[mask] > maxWeight || o.volume[mask] > maxVolume {
			o.valid[mask] = false
			continue
		}

		// Then check hazmat and route compatibility
		o.valid[mask] = o.isValidSubset(mask)
	}
}

// isValidSubset checks if a subset of orders is compatible
func (o *Optimizer) isValidSubset(mask int) bool {
	if mask == 0 {
		return true
	}

	var hasHazmat, hasNonHazmat bool
	var origin, destination string

	for i := 0; i < o.n; i++ {
		if mask&(1<<i) == 0 {
			continue
		}
		order := o.orders[i]

		// Check hazmat compatibility
		if order.IsHazmat {
			hasHazmat = true
		} else {
			hasNonHazmat = true
		}

		// All orders must have same origin/destination
		if origin == "" {
			origin = order.Origin
			destination = order.Destination
		} else {
			if !stringsEqualFold(origin, order.Origin) {
				return false
			}
			if !stringsEqualFold(destination, order.Destination) {
				return false
			}
		}
	}

	// Hazmat can only be with hazmat
	if hasHazmat && hasNonHazmat {
		return false
	}

	return true
}

// FindOptimal finds the best subset using DP
// Capacity constraints already checked during precompute via pruning
func (o *Optimizer) FindOptimal() int {
	bestMask := 0
	bestPayout := int64(0)

	// Iterate through all subsets
	for mask := 1; mask < o.maxMask; mask++ {
		if !o.valid[mask] {
			continue
		}
		if o.payout[mask] > bestPayout {
			bestPayout = o.payout[mask]
			bestMask = mask
		}
	}

	return bestMask
}

// BuildResponse creates the response from the best mask
func (o *Optimizer) BuildResponse(bestMask int) *OptimizeResponse {
	orderIDs := []string{}
	for i := 0; i < o.n; i++ {
		if bestMask&(1<<i) != 0 {
			orderIDs = append(orderIDs, o.orders[i].ID)
		}
	}

	weightPct := 0.0
	volumePct := 0.0
	if o.truck.MaxWeightLbs > 0 {
		weightPct = float64(o.weight[bestMask]) / float64(o.truck.MaxWeightLbs) * 100
	}
	if o.truck.MaxVolumeCuft > 0 {
		volumePct = float64(o.volume[bestMask]) / float64(o.truck.MaxVolumeCuft) * 100
	}

	return &OptimizeResponse{
		TruckID:                  o.truck.ID,
		SelectedOrderIDs:         orderIDs,
		TotalPayoutCents:         o.payout[bestMask],
		TotalWeightLbs:           o.weight[bestMask],
		TotalVolumeCuft:          o.volume[bestMask],
		UtilizationWeightPercent: roundTo2Decimals(weightPct),
		UtilizationVolumePercent: roundTo2Decimals(volumePct),
	}
}

// bitPosition returns the position of the single set bit (0-indexed)
func bitPosition(x int) int {
	pos := 0
	for x > 1 {
		x >>= 1
		pos++
	}
	return pos
}

func stringsEqualFold(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func roundTo2Decimals(x float64) float64 {
	return float64(int64(x*100+0.5)) / 100
}
