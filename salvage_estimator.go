package main

import (
	"encoding/json"
	"math"
	"os"
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

// LoadSalvageConfig loads salvage configuration from JSON file
func LoadSalvageConfig(path string) (*SalvageTableConfig, map[string]float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}

	var rawConfig map[string]interface{}
	if err := json.Unmarshal(data, &rawConfig); err != nil {
		return nil, nil, err
	}

	// Extract salvage table config
	salvageTableData := rawConfig["salvage_table_config"].(map[string]interface{})
	cfg := &SalvageTableConfig{
		MaxWeeks:      20,
		MaxInventory:  10000,
		InventoryStep: 100,
		ScrapFraction: salvageTableData["scrap_fraction"].(float64),
		SigmaNoise:    salvageTableData["sigma_noise"].(float64),
		MarkdownMin:   salvageTableData["markdown_min"].(float64),
		MarkdownMax:   salvageTableData["markdown_max"].(float64),
		UrgencyScale:  salvageTableData["urgency_scale"].(float64),
	}

	// Extract category elasticity priors
	elasticityData := rawConfig["category_elasticity_priors"].(map[string]interface{})
	elasticities := make(map[string]float64)
	for cat, val := range elasticityData {
		elasticities[cat] = val.(float64)
	}

	return cfg, elasticities, nil
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

// CategoryElasticityPriors is loaded from config at runtime (no hardcoded values)
var CategoryElasticityPriors map[string]float64

// ElasticityLearner updates elasticity beliefs from observations
type ElasticityLearner struct {
	SigmaNoise float64
}

// NewElasticityLearner creates a new learner with sigma noise from loaded config
func NewElasticityLearner(sigmaNoise float64) *ElasticityLearner {
	if sigmaNoise <= 0 {
		sigmaNoise = 0.30 // fallback default
	}
	return &ElasticityLearner{SigmaNoise: sigmaNoise}
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

