package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

// ItemCatalogFeatures represents input item catalog data
type ItemCatalogFeatures struct {
	ASIN                 string
	Category             string
	Subcategory          string
	WeeklyVelocityUnits  float64
	DemandCV             float64 // -1 if unknown
	Price                float64
}

// CIVEstimate represents output: estimated lambda value
type CIVEstimate struct {
	ASIN              string
	LambdaValue       float64
	CIVScore          float64
	VelocityScore     float64
	StabilityScore    float64
	EssentialityScore float64
	Confidence        float64 // [0,1]
	ComputedAt        time.Time
	DataVersion       string
}

// DataSourceConfig specifies where to load event data from
type DataSourceConfig struct {
	Type               string `json:"type"`                 // "file" or "api"
	CheckoutEventsPath string `json:"checkout_events_path"`
	DropoutEventsPath  string `json:"dropout_events_path"`
	APIEndpoint        string `json:"api_endpoint"`
	APIToken           string `json:"api_token"`
}

// FallbackThresholds defines default values when data is unavailable
type FallbackThresholds struct {
	ConfidenceThreshold        float64 `json:"confidence_threshold"`
	DefaultStabilityScore      float64 `json:"default_stability_score"`
	DefaultEssentialityScore   float64 `json:"default_essentiality_score"`
	DefaultP90Velocity         float64 `json:"default_p90_velocity"`
	MinCooccurrenceSupport     int     `json:"min_cooccurrence_support"`
}

// CheckoutEvent represents a completed basket transaction
type CheckoutEvent struct {
	SessionID   string   `json:"session_id"`
	WeekLabel   string   `json:"week_label"` // e.g. "2026-W01"
	BasketASINs []string `json:"basket_asins"`
	Timestamp   string   `json:"timestamp"`
}

// DropoutEvent represents an abandoned basket due to OOS item
type DropoutEvent struct {
	SessionID   string   `json:"session_id"`
	BasketASINs []string `json:"basket_asins"`
	OOSItem     string   `json:"oos_item"`
	Timestamp   string   `json:"timestamp"`
}

// EventLoader interface abstracts loading checkout and dropout events
type EventLoader interface {
	LoadCheckoutEvents() ([]CheckoutEvent, error)
	LoadDropoutEvents() ([]DropoutEvent, error)
}

// FileEventLoader loads events from JSON files
type FileEventLoader struct {
	CheckoutPath string
	DropoutPath  string
}

// APIEventLoader loads events from API (stub for now)
type APIEventLoader struct {
	Endpoint string
	Token    string
}

// ScoreAggregator computes item scores from events
type ScoreAggregator struct{}

// DemoCatalogItem is an intermediate struct for JSON unmarshaling
type DemoCatalogItem struct {
	ASIN                 string  `json:"asin"`
	Category             string  `json:"category"`
	Subcategory          string  `json:"subcategory"`
	WeeklyVelocityUnits  float64 `json:"weekly_velocity_units"`
	DemandCV             float64 `json:"demand_cv"`
	Price                float64 `json:"price"`
}

// CIVConfig holds configuration for CIV computation
type CIVConfig struct {
	LambdaMin          float64            `json:"lambda_min"`
	LambdaMax          float64            `json:"lambda_max"`
	WeightVelocity     float64            `json:"weight_velocity"`
	WeightStability    float64            `json:"weight_stability"`
	WeightEssentiality float64            `json:"weight_essentiality"`
	FallbackLambda     float64            `json:"fallback_lambda"`
	DataSource         DataSourceConfig   `json:"data_source"`
	FallbackThresholds FallbackThresholds `json:"fallback_thresholds"`
	// Runtime-populated maps (not in JSON)
	ItemEssentiality   map[string]float64
	ItemVelocityScore  map[string]float64
	ItemStabilityScore map[string]float64
	ItemAssociations   map[string]map[string]float64
}

// DefaultCIVConfig returns default configuration (no score maps, these are loaded from events)
func DefaultCIVConfig() *CIVConfig {
	return &CIVConfig{
		LambdaMin:          0.10,
		LambdaMax:          3.00,
		WeightVelocity:     0.35,
		WeightStability:    0.25,
		WeightEssentiality: 0.40,
		FallbackLambda:     0.87,
		FallbackThresholds: FallbackThresholds{
			ConfidenceThreshold:      0.33,
			DefaultStabilityScore:    0.80,
			DefaultEssentialityScore: 0.45,
			DefaultP90Velocity:       50.0,
			MinCooccurrenceSupport:   2,
		},
		ItemEssentiality:   make(map[string]float64),
		ItemVelocityScore:  make(map[string]float64),
		ItemStabilityScore: make(map[string]float64),
		ItemAssociations:   make(map[string]map[string]float64),
	}
}

// LoadCheckoutEvents loads checkout events from JSON file
func (f *FileEventLoader) LoadCheckoutEvents() ([]CheckoutEvent, error) {
	data, err := os.ReadFile(f.CheckoutPath)
	if err != nil {
		return nil, err
	}
	var events []CheckoutEvent
	if err := json.Unmarshal(data, &events); err != nil {
		return nil, err
	}
	return events, nil
}

// LoadDropoutEvents loads dropout events from JSON file
func (f *FileEventLoader) LoadDropoutEvents() ([]DropoutEvent, error) {
	data, err := os.ReadFile(f.DropoutPath)
	if err != nil {
		return nil, err
	}
	var events []DropoutEvent
	if err := json.Unmarshal(data, &events); err != nil {
		return nil, err
	}
	return events, nil
}

// LoadCheckoutEvents stub for API loader
func (a *APIEventLoader) LoadCheckoutEvents() ([]CheckoutEvent, error) {
	return nil, fmt.Errorf("API event loader not yet implemented")
}

// LoadDropoutEvents stub for API loader
func (a *APIEventLoader) LoadDropoutEvents() ([]DropoutEvent, error) {
	return nil, fmt.Errorf("API event loader not yet implemented")
}

// Compute aggregates events into item scores using provided thresholds
func (sa *ScoreAggregator) Compute(checkouts []CheckoutEvent, dropouts []DropoutEvent, thresholds FallbackThresholds) (
	essentiality, velocityScore, stabilityScore map[string]float64,
	associations map[string]map[string]float64,
) {
	essentiality = make(map[string]float64)
	velocityScore = make(map[string]float64)
	stabilityScore = make(map[string]float64)
	associations = make(map[string]map[string]float64)

	checkoutCount := make(map[string]int)
	weeklyCount := make(map[string]map[string]int) // asin -> weekLabel -> count
	dropoutCaused := make(map[string]int)
	cooccurrence := make(map[string]map[string]int) // A -> B -> count
	basketCount := make(map[string]int)             // items per basket (for association normalization)

	// Step 1: Build checkout frequency maps
	for _, event := range checkouts {
		for _, asin := range event.BasketASINs {
			checkoutCount[asin]++
			if weeklyCount[asin] == nil {
				weeklyCount[asin] = make(map[string]int)
			}
			weeklyCount[asin][event.WeekLabel]++
		}
		// Count basket-level items for association normalization
		for _, asin := range event.BasketASINs {
			basketCount[asin]++
		}
		// Build co-occurrence matrix
		for i, asinA := range event.BasketASINs {
			if associations[asinA] == nil {
				associations[asinA] = make(map[string]float64)
			}
			for j, asinB := range event.BasketASINs {
				if i != j {
					if cooccurrence[asinA] == nil {
						cooccurrence[asinA] = make(map[string]int)
					}
					cooccurrence[asinA][asinB]++
				}
			}
		}
	}

	// Step 2: Compute velocity scores
	maxCount := 0
	for _, count := range checkoutCount {
		if count > maxCount {
			maxCount = count
		}
	}
	if maxCount > 0 {
		for asin, count := range checkoutCount {
			velocityScore[asin] = float64(count) / float64(maxCount)
		}
	}

	// Step 3: Compute stability scores
	for asin, weekCounts := range weeklyCount {
		if len(weekCounts) == 0 {
			stabilityScore[asin] = 1.0
			continue
		}
		counts := make([]float64, 0)
		sum := 0.0
		for _, count := range weekCounts {
			counts = append(counts, float64(count))
			sum += float64(count)
		}
		mean := sum / float64(len(counts))
		if mean == 0 {
			stabilityScore[asin] = 1.0
			continue
		}
		variance := 0.0
		for _, count := range counts {
			variance += (count - mean) * (count - mean)
		}
		variance /= float64(len(counts))
		stddev := math.Sqrt(variance)
		demandCV := stddev / mean
		stabilityScore[asin] = 1.0 / (1.0 + demandCV)
	}

	// Step 4: Build dropout counts
	for _, event := range dropouts {
		dropoutCaused[event.OOSItem]++
	}

	// Step 5: Compute essentiality scores
	allASINs := make(map[string]bool)
	for asin := range checkoutCount {
		allASINs[asin] = true
	}
	for asin := range dropoutCaused {
		allASINs[asin] = true
	}
	for asin := range allASINs {
		total := dropoutCaused[asin] + checkoutCount[asin]
		if total > 0 {
			essentiality[asin] = float64(dropoutCaused[asin]) / float64(total)
		} else {
			essentiality[asin] = 0.0
		}
	}

	// Step 6: Compute associations (co-occurrence with minimum support from config)
	minSupport := thresholds.MinCooccurrenceSupport
	for asinA, targets := range cooccurrence {
		if associations[asinA] == nil {
			associations[asinA] = make(map[string]float64)
		}
		for asinB, count := range targets {
			if count >= minSupport && basketCount[asinA] > 0 {
				associations[asinA][asinB] = float64(count) / float64(basketCount[asinA])
			}
		}
	}

	return essentiality, velocityScore, stabilityScore, associations
}

// LoadCIVConfig loads configuration from JSON and computes scores from events
func LoadCIVConfig(path string) (*CIVConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := DefaultCIVConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	var loader EventLoader
	switch cfg.DataSource.Type {
	case "file":
		loader = &FileEventLoader{
			CheckoutPath: cfg.DataSource.CheckoutEventsPath,
			DropoutPath:  cfg.DataSource.DropoutEventsPath,
		}
	case "api":
		loader = &APIEventLoader{
			Endpoint: cfg.DataSource.APIEndpoint,
			Token:    cfg.DataSource.APIToken,
		}
	default:
		return nil, fmt.Errorf("unknown data source type: %s", cfg.DataSource.Type)
	}

	checkouts, err := loader.LoadCheckoutEvents()
	if err != nil {
		return nil, fmt.Errorf("failed to load checkout events: %w", err)
	}
	dropouts, err := loader.LoadDropoutEvents()
	if err != nil {
		return nil, fmt.Errorf("failed to load dropout events: %w", err)
	}

	agg := &ScoreAggregator{}
	essentiality, velocityScore, stabilityScore, associations := agg.Compute(checkouts, dropouts, cfg.FallbackThresholds)
	cfg.ItemEssentiality = essentiality
	cfg.ItemVelocityScore = velocityScore
	cfg.ItemStabilityScore = stabilityScore
	cfg.ItemAssociations = associations

	return cfg, nil
}

// CIVEstimator computes customer in-stock value
type CIVEstimator struct {
	Config *CIVConfig
}

// NewCIVEstimator creates a new estimator with default config
func NewCIVEstimator(cfg *CIVConfig) *CIVEstimator {
	if cfg == nil {
		cfg = DefaultCIVConfig()
	}
	return &CIVEstimator{Config: cfg}
}

// ComputeCIV computes CIV for a single item
func (e *CIVEstimator) ComputeCIV(features *ItemCatalogFeatures, categoryP90Velocity map[string]float64) *CIVEstimate {
	if categoryP90Velocity == nil {
		categoryP90Velocity = make(map[string]float64)
	}

	normalizedCategory := strings.ToLower(features.Category)

	// Velocity score: ASIN-keyed lookup first, fall back to P90 method
	var velocityScore float64
	if score, ok := e.Config.ItemVelocityScore[features.ASIN]; ok {
		velocityScore = score
	} else {
		p90 := categoryP90Velocity[normalizedCategory]
		if p90 <= 0 {
			p90 = e.Config.FallbackThresholds.DefaultP90Velocity
		}
		velocityScore = features.WeeklyVelocityUnits / p90
		if velocityScore > 1.0 {
			velocityScore = 1.0
		}
		if velocityScore < 0 {
			velocityScore = 0.0
		}
	}

	// Stability score: ASIN-keyed lookup first, fall back to DemandCV, then default from config
	var stabilityScore float64
	if score, ok := e.Config.ItemStabilityScore[features.ASIN]; ok {
		stabilityScore = score
	} else if features.DemandCV >= 0 {
		stabilityScore = 1.0 / (1.0 + features.DemandCV)
	} else {
		stabilityScore = e.Config.FallbackThresholds.DefaultStabilityScore
	}

	// Essentiality score: ASIN-keyed lookup, fall back to config default
	essentialityScore, ok := e.Config.ItemEssentiality[features.ASIN]
	if !ok {
		essentialityScore = e.Config.FallbackThresholds.DefaultEssentialityScore
	}

	// Confidence: count how many signals are available
	nKnown := 0
	if features.WeeklyVelocityUnits > 0 {
		nKnown++
	} else if _, hasVelocity := e.Config.ItemVelocityScore[features.ASIN]; hasVelocity {
		nKnown++
	}
	if features.DemandCV >= 0 {
		nKnown++
	} else if _, hasStability := e.Config.ItemStabilityScore[features.ASIN]; hasStability {
		nKnown++
	}
	if _, hasEssentiality := e.Config.ItemEssentiality[features.ASIN]; hasEssentiality {
		nKnown++
	}
	confidence := float64(nKnown) / 3.0

	// Handle fully unknown item (confidence below configured threshold)
	if confidence < e.Config.FallbackThresholds.ConfidenceThreshold {
		return &CIVEstimate{
			ASIN:              features.ASIN,
			LambdaValue:       e.Config.FallbackLambda,
			CIVScore:          0.0,
			VelocityScore:     0.0,
			StabilityScore:    0.0,
			EssentialityScore: 0.0,
			Confidence:        0.0,
			ComputedAt:        time.Now(),
			DataVersion:       "v2",
		}
	}

	// Composite CIV score
	civScore := e.Config.WeightVelocity*velocityScore +
		e.Config.WeightStability*stabilityScore +
		e.Config.WeightEssentiality*essentialityScore

	// Scale to lambda range
	lambdaValue := e.Config.LambdaMin + civScore*(e.Config.LambdaMax-e.Config.LambdaMin)

	return &CIVEstimate{
		ASIN:              features.ASIN,
		LambdaValue:       lambdaValue,
		CIVScore:          civScore,
		VelocityScore:     velocityScore,
		StabilityScore:    stabilityScore,
		EssentialityScore: essentialityScore,
		Confidence:        confidence,
		ComputedAt:        time.Now(),
		DataVersion:       "v2",
	}
}

// BatchComputeCIV computes CIV for multiple items
func (e *CIVEstimator) BatchComputeCIV(featuresList []*ItemCatalogFeatures) map[string]*CIVEstimate {
	// Compute category P90 velocities
	categoryVelocities := make(map[string][]float64)
	for _, feat := range featuresList {
		if feat.WeeklyVelocityUnits > 0 {
			normalizedCategory := strings.ToLower(feat.Category)
			categoryVelocities[normalizedCategory] = append(categoryVelocities[normalizedCategory], feat.WeeklyVelocityUnits)
		}
	}

	categoryP90 := make(map[string]float64)
	for cat, velocities := range categoryVelocities {
		if len(velocities) > 0 {
			sort.Float64s(velocities)
			idx := len(velocities) - 1
			if p90Idx := int(0.90 * float64(len(velocities))); p90Idx < len(velocities) && p90Idx > idx {
				idx = p90Idx
			}
			categoryP90[cat] = velocities[idx]
		} else {
			categoryP90[cat] = e.Config.FallbackThresholds.DefaultP90Velocity
		}
	}

	// Compute CIV for each item
	results := make(map[string]*CIVEstimate)
	for _, feat := range featuresList {
		est := e.ComputeCIV(feat, categoryP90)
		results[feat.ASIN] = est
	}

	return results
}

// loadDemoCatalog loads demo catalog from JSON file
func loadDemoCatalog(path string) ([]*ItemCatalogFeatures, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var items []DemoCatalogItem
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, err
	}
	catalog := make([]*ItemCatalogFeatures, len(items))
	for i, item := range items {
		catalog[i] = &ItemCatalogFeatures{
			ASIN:                item.ASIN,
			Category:            item.Category,
			Subcategory:         item.Subcategory,
			WeeklyVelocityUnits: item.WeeklyVelocityUnits,
			DemandCV:            item.DemandCV,
			Price:               item.Price,
		}
	}
	return catalog, nil
}

