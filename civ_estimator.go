package main

import (
	"fmt"
	"strings"
	"sort"
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

// CIVConfig holds configuration for CIV computation
type CIVConfig struct {
	LambdaMin            float64
	LambdaMax            float64
	WeightVelocity       float64
	WeightStability      float64
	WeightEssentiality   float64
	FallbackLambda       float64
	CategoryEssentiality map[string]float64
	CategoryStabilityPriors map[string]float64
}

// DefaultCIVConfig returns default configuration
func DefaultCIVConfig() *CIVConfig {
	return &CIVConfig{
		LambdaMin:          0.10,
		LambdaMax:          3.00,
		WeightVelocity:     0.35,
		WeightStability:    0.25,
		WeightEssentiality: 0.40,
		FallbackLambda:     0.87,
		CategoryEssentiality: map[string]float64{
			"dairy":              1.00,
			"milk":               1.00,
			"eggs":               0.95,
			"butter":             0.90,
			"household_staples":  0.85,
			"paper_towels":       0.85,
			"laundry":            0.85,
			"pantry_staples":     0.80,
			"oil":                0.80,
			"sugar":              0.78,
			"flour":              0.78,
			"beverages":          0.70,
			"water":              0.70,
			"juice":              0.68,
			"personal_care":      0.65,
			"shampoo":            0.65,
			"toothpaste":         0.65,
			"snacks":             0.45,
			"candy":              0.40,
			"specialty":          0.35,
			"ethnic":             0.35,
			"seasonal":           0.20,
			"discretionary":      0.20,
		},
		CategoryStabilityPriors: map[string]float64{
			"dairy":              0.08,
			"milk":               0.08,
			"eggs":               0.10,
			"household_staples":  0.12,
			"pantry_staples":     0.15,
			"beverages":          0.18,
			"personal_care":      0.20,
			"snacks":             0.35,
			"seasonal":           0.40,
			"discretionary":      0.45,
		},
	}
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

	// Velocity score
	p90 := categoryP90Velocity[features.Category]
	if p90 <= 0 {
		p90 = 50.0
	}
	velocityScore := features.WeeklyVelocityUnits / p90
	if velocityScore > 1.0 {
		velocityScore = 1.0
	}
	if velocityScore < 0 {
		velocityScore = 0.0
	}

	// Stability score
	var stabilityScore float64
	if features.DemandCV >= 0 {
		stabilityScore = 1.0 / (1.0 + features.DemandCV)
	} else {
		priorCV := e.Config.CategoryStabilityPriors[features.Category]
		if priorCV == 0 {
			priorCV = 0.25
		}
		stabilityScore = 1.0 / (1.0 + priorCV)
	}

	// Essentiality score
	essentialityScore := e.Config.CategoryEssentiality[features.Category]
	if essentialityScore == 0 {
		essentialityScore = 0.45
	}

	// Confidence
	nKnown := 0
	if features.WeeklyVelocityUnits > 0 {
		nKnown++
	}
	if features.DemandCV >= 0 {
		nKnown++
	}
	if _, exists := e.Config.CategoryEssentiality[features.Category]; exists {
		nKnown++
	}
	confidence := float64(nKnown) / 3.0

	// Handle fully unknown item
	if confidence < 0.33 {
		return &CIVEstimate{
			ASIN:              features.ASIN,
			LambdaValue:       e.Config.FallbackLambda,
			CIVScore:          0.0,
			VelocityScore:     0.0,
			StabilityScore:    0.0,
			EssentialityScore: 0.0,
			Confidence:        0.0,
			ComputedAt:        time.Now(),
			DataVersion:       "v1",
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
		DataVersion:       "v1",
	}
}

// BatchComputeCIV computes CIV for multiple items
func (e *CIVEstimator) BatchComputeCIV(featuresList []*ItemCatalogFeatures) map[string]*CIVEstimate {
	// Compute category P90 velocities
	categoryVelocities := make(map[string][]float64)
	for _, feat := range featuresList {
		if feat.WeeklyVelocityUnits > 0 {
			categoryVelocities[feat.Category] = append(categoryVelocities[feat.Category], feat.WeeklyVelocityUnits)
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
			categoryP90[cat] = 50.0
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

// demoCIV demonstrates CIV estimation
func demoCIV() {
	estimator := NewCIVEstimator(nil)

	// Sample catalog
	catalog := []*ItemCatalogFeatures{
		{ASIN: "ASIN-001-MILK", Category: "Dairy", Subcategory: "milk", WeeklyVelocityUnits: 120.0, DemandCV: 0.08, Price: 3.50},
		{ASIN: "ASIN-002-EGGS", Category: "Dairy", Subcategory: "eggs", WeeklyVelocityUnits: 95.0, DemandCV: 0.10, Price: 4.20},
		{ASIN: "ASIN-003-BUTTER", Category: "Dairy", Subcategory: "butter", WeeklyVelocityUnits: 60.0, DemandCV: 0.09, Price: 5.00},
		{ASIN: "ASIN-004-BREAD", Category: "Pantry", Subcategory: "bread", WeeklyVelocityUnits: 85.0, DemandCV: 0.15, Price: 2.50},
		{ASIN: "ASIN-005-OIL", Category: "Pantry", Subcategory: "oil", WeeklyVelocityUnits: 40.0, DemandCV: 0.12, Price: 8.00},
		{ASIN: "ASIN-006-FLOUR", Category: "Pantry", Subcategory: "flour", WeeklyVelocityUnits: 35.0, DemandCV: 0.14, Price: 3.00},
		{ASIN: "ASIN-007-TOWELS", Category: "Household", Subcategory: "paper_towels", WeeklyVelocityUnits: 70.0, DemandCV: 0.20, Price: 12.00},
		{ASIN: "ASIN-008-SALT", Category: "Pantry", Subcategory: "salt", WeeklyVelocityUnits: 15.0, DemandCV: 0.25, Price: 2.00},
		{ASIN: "ASIN-009-SPICE", Category: "Pantry", Subcategory: "spice", WeeklyVelocityUnits: 5.0, DemandCV: 0.40, Price: 6.00},
		{ASIN: "ASIN-010-CANDY", Category: "Snacks", Subcategory: "candy", WeeklyVelocityUnits: 20.0, DemandCV: 0.35, Price: 1.50},
	}

	results := estimator.BatchComputeCIV(catalog)

	fmt.Println("\n" + strings.Repeat("=", 100))
	fmt.Println("DYNAMIC CIV ESTIMATION (Go) - CATALOG ANALYSIS")
	fmt.Println(strings.Repeat("=", 100))
	fmt.Printf("%-20s %-15s %-10s %-10s %-8s %-12s\n", "ASIN", "Category", "Velocity", "λ (CIV)", "Score", "Confidence")
	fmt.Println(strings.Repeat("-", 100))

	for _, asin := range []string{
		"ASIN-001-MILK", "ASIN-002-EGGS", "ASIN-003-BUTTER",
		"ASIN-004-BREAD", "ASIN-005-OIL", "ASIN-006-FLOUR",
		"ASIN-007-TOWELS", "ASIN-008-SALT", "ASIN-009-SPICE", "ASIN-010-CANDY",
	} {
		est := results[asin]
		feat := catalog[0]
		for _, f := range catalog {
			if f.ASIN == asin {
				feat = f
				break
			}
		}

		fmt.Printf("%-20s %-15s %10.0f $%7.2f  %6.2f  %6.0f%%\n",
			asin, feat.Category, feat.WeeklyVelocityUnits, est.LambdaValue, est.CIVScore, est.Confidence*100)
	}

	fmt.Println("\n" + strings.Repeat("=", 100))
	fmt.Println("INTERPRETATION")
	fmt.Println(strings.Repeat("=", 100))
	fmt.Println(`
λ (CIV) Column:
  - High λ (~$2.50+): Destination item; OOS loses entire basket
  - Medium λ (~$0.85): Core item; OOS loses some complementary purchases
  - Low λ (~$0.20): Filler item; OOS has minimal basket impact
`)
}

// Uncomment below to run demo with: go run civ_estimator.go
// func main() {
// 	demoCIV()
// }
