package main

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// ElasticityBelief represents Bayesian belief over price elasticity
type ElasticityBelief struct {
	ASIN              string
	MuLogElasticity   float64
	TauLogElasticity  float64 // Precision: 1/variance
	NObservations     int
	LastUpdated       time.Time
	CategoryPriorMu   float64
}

// MarkdownObservation represents an observed markdown-demand response
type MarkdownObservation struct {
	ASIN              string
	WeekOfYear        int
	MarkdownFraction  float64
	BaselineDemand    float64
	ObservedDemand    float64
	Timestamp         time.Time
}

// SalvageTableConfig holds configuration for table generation
type SalvageTableConfig struct {
	MaxWeeks      int
	MaxInventory  int
	InventoryStep int
	ScrapFraction float64
	SigmaNoise    float64
	MarkdownMin   float64
	MarkdownMax   float64
	UrgencyScale  float64
}

// DefaultSalvageTableConfig returns default configuration
func DefaultSalvageTableConfig() *SalvageTableConfig {
	return &SalvageTableConfig{
		MaxWeeks:      20,
		MaxInventory:  10000,
		InventoryStep: 100,
		ScrapFraction: 0.05,
		SigmaNoise:    0.30,
		MarkdownMin:   0.05,
		MarkdownMax:   0.50,
		UrgencyScale:  1.5,
	}
}

// SalvageEstimate represents output: salvage table and elasticity
type SalvageEstimate struct {
	ASIN                    string
	SalvageTable            map[int]map[int]float64
	ElasticityPointEstimate float64
	ElasticityConfidence    float64
	ComputedAt              time.Time
	BasedOnNObs             int
}

// Category elasticity priors (from retail economics literature)
var CategoryElasticityPriors = map[string]float64{
	"dairy":              -0.6,
	"milk":               -0.6,
	"eggs":               -0.65,
	"butter":             -0.70,
	"packaged_food":      -0.80,
	"pantry_staples":     -0.85,
	"oil":                -0.85,
	"flour":              -0.80,
	"household_staples":  -1.20,
	"paper_towels":       -1.20,
	"beverages":          -1.00,
	"water":              -1.50,
	"juice":              -0.95,
	"personal_care":      -1.50,
	"shampoo":            -1.50,
	"toothpaste":         -1.40,
	"snacks":             -1.80,
	"candy":              -2.00,
	"specialty":          -2.00,
	"seasonal":           -2.50,
	"discretionary":      -3.00,
}

// ElasticityLearner updates elasticity beliefs from observations
type ElasticityLearner struct {
	SigmaNoise float64
}

// NewElasticityLearner creates a new learner
func NewElasticityLearner() *ElasticityLearner {
	return &ElasticityLearner{SigmaNoise: 0.30}
}

// InitializeBelief creates prior belief from category
func (el *ElasticityLearner) InitializeBelief(asin, category string) *ElasticityBelief {
	epsilonPrior := CategoryElasticityPriors[category]
	if epsilonPrior == 0 {
		epsilonPrior = -1.0
	}

	mu0 := math.Log(-epsilonPrior)
	tau0 := 4.0 // Prior precision

	return &ElasticityBelief{
		ASIN:             asin,
		MuLogElasticity:  mu0,
		TauLogElasticity: tau0,
		NObservations:    0,
		LastUpdated:      time.Now(),
		CategoryPriorMu:  mu0,
	}
}

// UpdateBelief applies Bayesian update from an observation
func (el *ElasticityLearner) UpdateBelief(belief *ElasticityBelief, obs *MarkdownObservation) *ElasticityBelief {
	if obs.BaselineDemand <= 0 || obs.ObservedDemand <= 0 {
		return belief
	}

	deltaLogP := math.Log(1.0 - obs.MarkdownFraction)
	if math.Abs(deltaLogP) < 1e-6 {
		return belief
	}

	deltaLogQ := math.Log(obs.ObservedDemand / obs.BaselineDemand)
	epsilonObsLog := deltaLogQ / deltaLogP

	// Likelihood precision
	likelihoodTau := (deltaLogP * deltaLogP) / (el.SigmaNoise * el.SigmaNoise)

	// Conjugate update
	tauNew := belief.TauLogElasticity + likelihoodTau
	muNew := (belief.TauLogElasticity*belief.MuLogElasticity +
		likelihoodTau*epsilonObsLog) / tauNew

	return &ElasticityBelief{
		ASIN:             belief.ASIN,
		MuLogElasticity:  muNew,
		TauLogElasticity: tauNew,
		NObservations:    belief.NObservations + 1,
		LastUpdated:      time.Now(),
		CategoryPriorMu:  belief.CategoryPriorMu,
	}
}

// MarkdownAdvisor recommends markdown fractions
type MarkdownAdvisor struct{}

// RecommendMarkdown recommends markdown fraction for inventory management
func (ma *MarkdownAdvisor) RecommendMarkdown(
	invOnHand, forecastWeeklyDemand, weeksRemaining, elasticityEstimate, price, cost float64,
	config *SalvageTableConfig,
) float64 {
	if forecastWeeklyDemand <= 0 || weeksRemaining <= 0 {
		return 0.0
	}

	requiredWeeklyRate := invOnHand / weeksRemaining
	if requiredWeeklyRate <= forecastWeeklyDemand {
		return 0.0
	}

	demandLiftNeeded := requiredWeeklyRate / forecastWeeklyDemand
	exponent := 1.0 / elasticityEstimate
	mStar := 1.0 - math.Exp(exponent*math.Log(demandLiftNeeded))
	mStar = math.Max(mStar, 0.0)

	// Urgency adjustment
	timeRatio := weeksRemaining / float64(config.MaxWeeks)
	if timeRatio < 0 {
		timeRatio = 0
	}
	urgency := 1.0 + math.Pow(1.0-timeRatio, 2)*config.UrgencyScale
	mAdjusted := mStar * urgency

	// Feasibility
	mCostFloor := config.MarkdownMax
	if price > 0 {
		mCostFloor = 1.0 - cost/price
	}

	mFinal := math.Max(config.MarkdownMin,
		math.Min(mAdjusted,
			math.Min(config.MarkdownMax, mCostFloor)))

	return mFinal
}

// SalvageGenerator generates salvage tables
type SalvageGenerator struct {
	Config           *SalvageTableConfig
	MarkdownAdvisor  *MarkdownAdvisor
}

// NewSalvageGenerator creates a new generator
func NewSalvageGenerator(config *SalvageTableConfig) *SalvageGenerator {
	if config == nil {
		config = DefaultSalvageTableConfig()
	}
	return &SalvageGenerator{
		Config:          config,
		MarkdownAdvisor: &MarkdownAdvisor{},
	}
}

// GenerateSalvageTable generates 2D salvage table
func (sg *SalvageGenerator) GenerateSalvageTable(
	asin string,
	price, cost, demandMeanWeekly float64,
	belief *ElasticityBelief,
) *SalvageEstimate {

	elasticity := -math.Exp(belief.MuLogElasticity)
	confidence := math.Sqrt(belief.TauLogElasticity / 4.0)

	table := make(map[int]map[int]float64)

	for week := 0; week <= sg.Config.MaxWeeks; week++ {
		table[week] = make(map[int]float64)

		for invLevel := 0; invLevel <= sg.Config.MaxInventory; invLevel += sg.Config.InventoryStep {
			if invLevel == 0 {
				table[week][invLevel] = 0.0
				continue
			}

			// Simulate sell-down
			invRemaining := float64(invLevel)
			totalRevenue := 0.0

			for w := week; w >= 0; w-- {
				if invRemaining <= 1e-6 {
					break
				}

				weeksUntilDeadline := float64(w + 1)
				m := sg.MarkdownAdvisor.RecommendMarkdown(
					invRemaining, demandMeanWeekly, weeksUntilDeadline,
					elasticity, price, cost, sg.Config,
				)

				demandAtMarkdown := demandMeanWeekly * math.Pow(1.0-m, -elasticity)
				unitsSold := math.Min(invRemaining, demandAtMarkdown)
				revenue := unitsSold * price * (1.0 - m)
				totalRevenue += revenue
				invRemaining -= unitsSold
			}

			// Scrap unsold
			scrapValue := invRemaining * cost * sg.Config.ScrapFraction
			table[week][invLevel] = totalRevenue + scrapValue
		}
	}

	return &SalvageEstimate{
		ASIN:                    asin,
		SalvageTable:            table,
		ElasticityPointEstimate: elasticity,
		ElasticityConfidence:    confidence,
		ComputedAt:              time.Now(),
		BasedOnNObs:             belief.NObservations,
	}
}

func main() {
	demoSalvage()
}

// demoSalvage demonstrates salvage estimation
func demoSalvage() {
	learner := NewElasticityLearner()

	// Initialize belief
	belief := learner.InitializeBelief("ASIN-001-MILK", "Dairy")
	fmt.Println("\nInitial belief for Dairy item:")
	fmt.Printf("  Category prior: epsilon = -0.6\n")
	fmt.Printf("  mu_log_epsilon = %.4f\n", belief.MuLogElasticity)
	fmt.Printf("  precision tau = %.2f\n", belief.TauLogElasticity)
	fmt.Printf("  Estimated epsilon: %.2f\n\n", -math.Exp(belief.MuLogElasticity))

	// Simulate observations
	fmt.Println(strings.Repeat("-", 100))
	fmt.Println("Simulated markdown observations (true epsilon = -1.0):")
	fmt.Println(strings.Repeat("-", 100))
	fmt.Printf("%-4s %-12s %-12s %-12s %-12s %-12s\n", "Obs", "Markdown", "Baseline", "Observed", "Updated ε", "Confidence")
	fmt.Println(strings.Repeat("-", 100))

	observations := []struct {
		markdown float64
		baseline float64
		observed float64
	}{
		{0.10, 100.0, 105.0},
		{0.15, 100.0, 110.0},
		{0.10, 100.0, 108.0},
		{0.20, 100.0, 118.0},
		{0.25, 100.0, 125.0},
		{0.15, 100.0, 112.0},
		{0.20, 100.0, 122.0},
		{0.10, 100.0, 107.0},
		{0.25, 100.0, 128.0},
		{0.30, 100.0, 135.0},
	}

	for i, obs := range observations {
		markdownObs := &MarkdownObservation{
			ASIN:              "ASIN-001-MILK",
			WeekOfYear:        i + 1,
			MarkdownFraction:  obs.markdown,
			BaselineDemand:    obs.baseline,
			ObservedDemand:    obs.observed,
			Timestamp:         time.Now(),
		}
		belief = learner.UpdateBelief(belief, markdownObs)
		epsilonEst := -math.Exp(belief.MuLogElasticity)
		confidenceVal := 1.0 / math.Sqrt(belief.TauLogElasticity)

		fmt.Printf("%-4d %10.0f%% %10.0f %10.0f %10.2f %10.3f\n",
			i+1, obs.markdown*100, obs.baseline, obs.observed, epsilonEst, confidenceVal)
	}

	fmt.Printf("\n%s\n", strings.Repeat("=", 100))
	fmt.Println("Final Bayesian estimate:")
	epsilonFinal := -math.Exp(belief.MuLogElasticity)
	stdErr := 1.0 / math.Sqrt(belief.TauLogElasticity)
	fmt.Printf("  Learned elasticity: %.2f\n", epsilonFinal)
	fmt.Printf("  Standard error: %.4f\n", stdErr)
	fmt.Printf("  Observations: %d\n", belief.NObservations)

	// Generate salvage table
	fmt.Printf("\n%s\n", strings.Repeat("=", 100))
	fmt.Println("SALVAGE TABLE GENERATION")
	fmt.Printf("%s\n\n", strings.Repeat("=", 100))

	generator := NewSalvageGenerator(nil)
	salvageEst := generator.GenerateSalvageTable("ASIN-001-MILK", 3.50, 1.50, 100.0, belief)

	fmt.Println("Generated salvage table for milk item:")
	fmt.Println("  Price: $3.50, Cost: $1.50, Weekly demand: 100 units")
	fmt.Printf("  Elasticity estimate: %.2f\n", salvageEst.ElasticityPointEstimate)
	fmt.Printf("  Based on %d observations\n\n", salvageEst.BasedOnNObs)

	fmt.Println("Sample salvage values:")
	fmt.Println(strings.Repeat("-", 70))
	fmt.Printf("%-8s %-20s %-15s\n", "Week", "Inventory Level", "Salvage Value")
	fmt.Println(strings.Repeat("-", 70))

	for _, week := range []int{0, 5, 10, 15, 20} {
		if weekTable, ok := salvageEst.SalvageTable[week]; ok {
			for _, invLevel := range []int{100, 500, 1000, 2000} {
				if val, ok := weekTable[invLevel]; ok {
					fmt.Printf("%-8d %-20d $%13.2f\n", week, invLevel, val)
				}
			}
		}
	}
}
