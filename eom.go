package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
)

// Distribution represents a probability distribution with quantiles
type Distribution struct {
	Quantiles []float64 // 50 quantile points
	Values    []float64 // corresponding values
}

// Item represents an SKU with all EOM parameters
type Item struct {
	ASIN string

	// Pricing parameters
	P      float64 // Sales price
	PPrime float64 // Additional CP terms on sale
	K      float64 // Penalty of lost sale
	C      float64 // Purchasing cost
	CPrime float64 // Additional CP terms on receipt
	A      float64 // Cost of arrival

	// Physical parameters
	V      float64 // Volume of item
	Lambda float64 // Consumer in-stock value (CIV)
	Alpha  float64 // CIV scaling factor
	H      float64 // Yearly cost of capital

	// Penalty parameters (for constraints)
	HBar  float64 // Per unit penalty
	HPrime float64 // Per volume penalty
	HHat  float64 // Per value unit penalty

	// Distributions
	VLTDist    *Distribution // Vendor Lead Time distribution (in days)
	DemandDist []*Distribution // Demand for each lead time (50 VLT scenarios)

	// Salvage function: r(l, y) -> salvage value given lead time l and inventory y
	// Represented as 2D grid: [lead_time_weeks][inventory_level]
	SalvageTable map[int]map[int]float64

	// Review period in days
	ReviewPeriod int

	// Current inventory on hand
	CurrentInventory float64
}

// EOAResult represents the result of EOM optimization
type EOAResult struct {
	ASIN           string
	OptimalTIP     float64
	MaxProfit      float64
	CriticalRatio  float64
	TargetServiceLevel float64
	Error          error
}

// EOMActor performs EOM calculations
type EOMActor struct {
	Bisection BisectionConfig
}

// BisectionConfig holds bisection algorithm parameters
type BisectionConfig struct {
	MaxIterations int
	Tolerance     float64
}

// DefaultBisectionConfig returns default bisection parameters
func DefaultBisectionConfig() BisectionConfig {
	return BisectionConfig{
		MaxIterations: 100,
		Tolerance:     1.0,
	}
}

// EOMMappingConfig holds all configurable parameters for EOM
type EOMMappingConfig struct {
	ItemParameters             map[string]interface{} `json:"item_parameters"`
	DistributionParameters     map[string]interface{} `json:"distribution_parameters"`
	SalvageTableParameters     map[string]interface{} `json:"salvage_table_parameters"`
	BisectionParameters        map[string]interface{} `json:"bisection_parameters"`
	DemoItems                  map[string]interface{} `json:"demo_items"`
}

// LoadEOMConfig loads EOM configuration from JSON file
func LoadEOMConfig(path string) (*EOMMappingConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg EOMMappingConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// CalculateUndarageCost computes cu = p + p' + k - (c - c') + α*Λ - a
func (item *Item) CalculateUnderageCost() float64 {
	return item.P + item.PPrime + item.K - (item.C - item.CPrime) + item.Alpha*item.Lambda - item.A
}

// CalculateOverageCost computes co = (c - c') + a + h̄ + h'*v + ĥ*(c - c')
func (item *Item) CalculateOverageCost() float64 {
	netCost := item.C - item.CPrime
	return netCost + item.A + item.HBar + item.HPrime*item.V + item.HHat*netCost
}

// CalculateHoldingCost computes hl = (γ^l - 1) where γ = 1 + h/365
func (item *Item) CalculateHoldingCost(leadDays int) float64 {
	gamma := 1.0 + item.H/365.0
	return math.Pow(gamma, float64(leadDays)) - 1.0
}

// GetSalvageValue returns salvage value at (leadTimeDays, leftoverInventory) using bilinear interpolation
func (item *Item) GetSalvageValue(leadTimeDays int, leftoverInventory float64) float64 {
	if leftoverInventory <= 0 {
		return 0
	}

	// Convert days to weeks for salvage table lookup
	weeks := float64(leadTimeDays) / 7.0

	// Check if exact week exists in salvage table
	week1 := int(weeks)
	week2 := week1 + 1
	fracWeek := weeks - float64(week1)

	// Get inventory levels from salvage table
	invLevel1 := int(leftoverInventory)
	invLevel2 := invLevel1 + 1
	fracInv := leftoverInventory - float64(invLevel1)

	// Bilinear interpolation
	val11 := item.getSalvageTableValue(week1, invLevel1)
	val12 := item.getSalvageTableValue(week1, invLevel2)
	val21 := item.getSalvageTableValue(week2, invLevel1)
	val22 := item.getSalvageTableValue(week2, invLevel2)

	// First interpolate along inventory axis for both weeks
	val1 := val11*(1-fracInv) + val12*fracInv
	val2 := val21*(1-fracInv) + val22*fracInv

	// Then interpolate along time axis
	result := val1*(1-fracWeek) + val2*fracWeek
	return math.Max(0, result) // Ensure non-negative
}

// Helper to safely get salvage table value with bounds checking
func (item *Item) getSalvageTableValue(week int, invLevel int) float64 {
	if weekMap, exists := item.SalvageTable[week]; exists {
		if val, exists := weekMap[invLevel]; exists {
			return val
		}
		// Return closest available value
		for i := invLevel; i >= 0; i-- {
			if val, exists := weekMap[i]; exists {
				return val
			}
		}
	}
	return 0
}

// ComputeObjectiveForTIP computes z(y) for TIP method
// z(y) = (1/2500) * Σ[cu*d - cu*(d-y)+ - (co + h_l*(c-c'))*(y-d)+ + r(l, (y-d)+)]
func (item *Item) ComputeObjectiveForTIP(targetY float64) float64 {
	cu := item.CalculateUnderageCost()
	co := item.CalculateOverageCost()
	netCost := item.C - item.CPrime

	totalProfit := 0.0
	count := 0.0

	// Iterate over VLT quantiles
	for vltIdx, vltVal := range item.VLTDist.Values {
		leadDays := int(vltVal)
		holdingCost := item.CalculateHoldingCost(leadDays)

		// Iterate over demand quantiles for this VLT
		if vltIdx < len(item.DemandDist) {
			for _, demandVal := range item.DemandDist[vltIdx].Values {
				demand := demandVal

				// Expected revenue from demand
				expectedRevenue := cu * demand

				// Underage cost: cu * max(demand - y, 0)
				undearageCostTerm := 0.0
				if demand > targetY {
					undearageCostTerm = cu * (demand - targetY)
				}

				// Overage cost: (co + h_l * (c - c')) * max(y - demand, 0)
				overageCostLinear := 0.0
				leftoverInv := 0.0
				if targetY > demand {
					leftoverInv = targetY - demand
					overageCostLinear = (co + holdingCost*netCost) * leftoverInv
				}

				// Salvage value recovery
				salvageValue := item.GetSalvageValue(leadDays, leftoverInv)

				// Profit for this realization
				profit := expectedRevenue - undearageCostTerm - overageCostLinear + salvageValue
				totalProfit += profit
				count += 1.0
			}
		}
	}

	if count > 0 {
		return totalProfit / count
	}
	return 0
}

// ComputeGradientForTIP computes dz/dy for TIP method (for bisection on gradient)
func (item *Item) ComputeGradientForTIP(targetY float64) float64 {
	cu := item.CalculateUnderageCost()
	co := item.CalculateOverageCost()
	netCost := item.C - item.CPrime

	totalGradient := 0.0
	count := 0.0

	for vltIdx, vltVal := range item.VLTDist.Values {
		leadDays := int(vltVal)
		holdingCost := item.CalculateHoldingCost(leadDays)

		if vltIdx < len(item.DemandDist) {
			for _, demandVal := range item.DemandDist[vltIdx].Values {
				demand := demandVal

				// dz/dy = -cu * P(D > y) + (co + h_l*(c-c')) * P(D < y) - dSalvage/dy
				probDemandGreater := 0.0
				if demand > targetY {
					probDemandGreater = 1.0
				}

				probDemandLess := 1.0 - probDemandGreater

				leftoverInv := math.Max(0, targetY-demand)

				// Approximate derivative of salvage value with respect to y
				// dSalvage/dy ≈ marginal salvage value (obtained from salvage table)
				deltaSalvage := 0.0
				if leftoverInv > 0 {
					// Approximate as linear decline
					sv1 := item.GetSalvageValue(leadDays, leftoverInv)
					sv2 := item.GetSalvageValue(leadDays, leftoverInv+1)
					deltaSalvage = sv2 - sv1
				}

				gradient := -cu*probDemandGreater + (co+holdingCost*netCost)*probDemandLess - deltaSalvage
				totalGradient += gradient
				count += 1.0
			}
		}
	}

	if count > 0 {
		return totalGradient / count
	}
	return 0
}

// ComputeCumulativeDistributionAtY computes H(y) = E_L[F_L(y)] for CR method
func (item *Item) ComputeCumulativeDistributionAtY(y float64) float64 {
	totalProb := 0.0
	count := 0.0

	for vltIdx := range item.VLTDist.Values {
		if vltIdx < len(item.DemandDist) {
			// Compute F_L(y) = P(D_L <= y)
			demandLessOrEqual := 0.0
			totalDemands := float64(len(item.DemandDist[vltIdx].Values))

			for _, demandVal := range item.DemandDist[vltIdx].Values {
				if demandVal <= y {
					demandLessOrEqual += 1.0
				}
			}

			if totalDemands > 0 {
				fL := demandLessOrEqual / totalDemands
				totalProb += fL
				count += 1.0
			}
		}
	}

	if count > 0 {
		return totalProb / count
	}
	return 0
}

// SolveEOMCR solves the CR method using bisection
// Finds y* such that H(y) >= targetServiceLevel
func (eom *EOMActor) SolveEOMCR(item *Item, targetServiceLevel float64) *EOAResult {
	result := &EOAResult{ASIN: item.ASIN, TargetServiceLevel: targetServiceLevel}

	// Bisection search for y where H(y) = targetServiceLevel
	left := 0.0
	right := 10000.0 // Assume reasonable upper bound

	// First find a right bound where H(right) >= targetServiceLevel
	for i := 0; i < 20; i++ {
		if item.ComputeCumulativeDistributionAtY(right) >= targetServiceLevel {
			break
		}
		right *= 2
	}

	for iter := 0; iter < eom.Bisection.MaxIterations; iter++ {
		mid := (left + right) / 2.0
		h := item.ComputeCumulativeDistributionAtY(mid)

		if math.Abs(h-targetServiceLevel) < 0.0001 {
			result.OptimalTIP = mid
			result.CriticalRatio = h
			result.MaxProfit = item.ComputeObjectiveForTIP(mid)
			// Verify at nearby integers for exact optimality
			return eom.verifyCRIntegerOptimality(item, result, targetServiceLevel, 5)
		}

		if h < targetServiceLevel {
			left = mid
		} else {
			right = mid
		}
	}

	result.OptimalTIP = (left + right) / 2.0
	result.CriticalRatio = item.ComputeCumulativeDistributionAtY(result.OptimalTIP)
	result.MaxProfit = item.ComputeObjectiveForTIP(result.OptimalTIP)
	// Verify at nearby integers for exact optimality
	return eom.verifyCRIntegerOptimality(item, result, targetServiceLevel, 5)
}

// SolveEOMTIP solves the TIP method using bisection on the gradient
// Finds y* that minimizes cost (maximizes profit)
func (eom *EOMActor) SolveEOMTIP(item *Item) *EOAResult {
	result := &EOAResult{ASIN: item.ASIN}

	// Bisection search for y where dz/dy = 0
	left := 0.0
	right := 10000.0

	// Find bounds where gradient changes sign
	_ = item.ComputeGradientForTIP(left)  // Left boundary check
	gradRight := item.ComputeGradientForTIP(right)

	// Expand right if necessary
	for gradRight < 0 && right < 100000 {
		right *= 2
		gradRight = item.ComputeGradientForTIP(right)
	}

	for iter := 0; iter < eom.Bisection.MaxIterations; iter++ {
		mid := (left + right) / 2.0
		grad := item.ComputeGradientForTIP(mid)

		if math.Abs(grad) < eom.Bisection.Tolerance {
			result.OptimalTIP = mid
			result.MaxProfit = item.ComputeObjectiveForTIP(mid)
			result.CriticalRatio = item.ComputeCumulativeDistributionAtY(mid)
			// Verify at nearby integers for exact optimality
			return eom.verifyIntegerOptimality(item, result, 5)
		}

		if grad < 0 {
			left = mid
		} else {
			right = mid
		}
	}

	result.OptimalTIP = (left + right) / 2.0
	result.MaxProfit = item.ComputeObjectiveForTIP(result.OptimalTIP)
	result.CriticalRatio = item.ComputeCumulativeDistributionAtY(result.OptimalTIP)
	// Verify at nearby integers for exact optimality
	return eom.verifyIntegerOptimality(item, result, 5)
}

// verifyIntegerOptimality checks nearby integers to find the best integer solution for TIP
// This ensures the final result is optimal when you need whole units (integer inventory)
func (eom *EOMActor) verifyIntegerOptimality(item *Item, result *EOAResult, verifyRange int) *EOAResult {
	// Round to nearest integer
	yCenter := int(math.Round(result.OptimalTIP))
	bestY := float64(yCenter)
	bestProfit := item.ComputeObjectiveForTIP(bestY)

	// Check range of integers around optimum
	start := yCenter - verifyRange
	if start < 0 {
		start = 0
	}
	end := yCenter + verifyRange

	// Evaluate all nearby integers and pick the best (maximum profit)
	for y := start; y <= end; y++ {
		yFloat := float64(y)
		profit := item.ComputeObjectiveForTIP(yFloat)
		if profit > bestProfit {
			bestProfit = profit
			bestY = yFloat
		}
	}

	// Update result with best integer found
	result.OptimalTIP = bestY
	result.MaxProfit = item.ComputeObjectiveForTIP(bestY)
	result.CriticalRatio = item.ComputeCumulativeDistributionAtY(bestY)
	return result
}

// verifyCRIntegerOptimality checks nearby integers for CR method
// Finds minimum integer y that meets the service level target
func (eom *EOMActor) verifyCRIntegerOptimality(item *Item, result *EOAResult, targetServiceLevel float64, verifyRange int) *EOAResult {
	// Round to nearest integer
	yCenter := int(math.Round(result.OptimalTIP))

	// Check range and find all integers meeting service level
	start := yCenter - verifyRange
	if start < 0 {
		start = 0
	}
	end := yCenter + verifyRange

	var bestY int = yCenter
	found := false

	// Find minimum y that meets service level requirement
	for y := start; y <= end; y++ {
		sl := item.ComputeCumulativeDistributionAtY(float64(y))
		if sl >= targetServiceLevel {
			bestY = y
			found = true
			break // Found minimum, no need to continue
		}
	}

	// If nothing found in range, use original
	if !found {
		bestY = yCenter
	}

	// Update result with best integer found
	result.OptimalTIP = float64(bestY)
	result.MaxProfit = item.ComputeObjectiveForTIP(float64(bestY))
	result.CriticalRatio = item.ComputeCumulativeDistributionAtY(float64(bestY))
	return result
}

// ProcessItemsConcurrently processes multiple items in parallel
func ProcessItemsConcurrently(items []*Item, numWorkers int, useTIP bool, serviceLevelForCR float64) []*EOAResult {
	results := make([]*EOAResult, len(items))
	resultChan := make(chan struct{ idx int; result *EOAResult }, len(items))
	var wg sync.WaitGroup

	eom := &EOMActor{Bisection: DefaultBisectionConfig()}

	// Worker pool
	itemChan := make(chan struct{ idx int; item *Item }, len(items))
	wg.Add(numWorkers)

	for w := 0; w < numWorkers; w++ {
		go func() {
			defer wg.Done()
			for work := range itemChan {
				var result *EOAResult
				if useTIP {
					result = eom.SolveEOMTIP(work.item)
				} else {
					result = eom.SolveEOMCR(work.item, serviceLevelForCR)
				}
				resultChan <- struct{ idx int; result *EOAResult }{work.idx, result}
			}
		}()
	}

	// Send work
	go func() {
		for idx, item := range items {
			itemChan <- struct{ idx int; item *Item }{idx, item}
		}
		close(itemChan)
	}()

	// Collect results
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	for res := range resultChan {
		results[res.idx] = res.result
	}

	return results
}


// LoadDemoItems loads demo items from JSON configuration
func LoadDemoItems(configPath string) ([]*Item, error) {
	cfg, err := LoadEOMConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load EOM config: %w", err)
	}

	// Ensure demo_items path is set
	var demoPath string
	if demoItems, ok := cfg.DemoItems["data_source"].(string); ok {
		demoPath = demoItems
	} else {
		demoPath = "data/items/demo_items.json"
	}

	data, err := os.ReadFile(demoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read demo items file: %w", err)
	}

	type DemoItemData struct {
		ASIN             string  `json:"asin"`
		Description      string  `json:"description"`
		P                float64 `json:"p"`
		PPrime           float64 `json:"p_prime"`
		K                float64 `json:"k"`
		C                float64 `json:"c"`
		CPrime           float64 `json:"c_prime"`
		A                float64 `json:"a"`
		V                float64 `json:"v"`
		Lambda           float64 `json:"lambda"`
		Alpha            float64 `json:"alpha"`
		H                float64 `json:"h"`
		HBar             float64 `json:"h_bar"`
		HPrime           float64 `json:"h_prime"`
		HHat             float64 `json:"h_hat"`
		ReviewPeriodDays int     `json:"review_period_days"`
		CurrentInventory float64 `json:"current_inventory"`
	}

	var demoItemsData []DemoItemData
	if err := json.Unmarshal(data, &demoItemsData); err != nil {
		return nil, fmt.Errorf("failed to parse demo items JSON: %w", err)
	}

	items := make([]*Item, len(demoItemsData))
	for i, d := range demoItemsData {
		item := &Item{
			ASIN:             d.ASIN,
			P:                d.P,
			PPrime:           d.PPrime,
			K:                d.K,
			C:                d.C,
			CPrime:           d.CPrime,
			A:                d.A,
			V:                d.V,
			Lambda:           d.Lambda,
			Alpha:            d.Alpha,
			H:                d.H,
			HBar:             d.HBar,
			HPrime:           d.HPrime,
			HHat:             d.HHat,
			ReviewPeriod:     d.ReviewPeriodDays,
			CurrentInventory: d.CurrentInventory,
		}

		// Generate placeholder distributions (VLT and demand)
		// These should ideally be loaded from config or data files in future
		item.VLTDist = &Distribution{
			Quantiles: make([]float64, 50),
			Values:    make([]float64, 50),
		}
		for j := 0; j < 50; j++ {
			item.VLTDist.Quantiles[j] = 2.0 + float64(j)*(96.0/48.0)
			item.VLTDist.Values[j] = 5.0 + float64(j)*0.4 // Approximate: 5-25 days
		}

		item.DemandDist = make([]*Distribution, 50)
		for j := 0; j < 50; j++ {
			item.DemandDist[j] = &Distribution{
				Quantiles: make([]float64, 50),
				Values:    make([]float64, 50),
			}
			baseDemand := 400.0 + float64(j)*2.0
			for k := 0; k < 50; k++ {
				item.DemandDist[j].Quantiles[k] = 2.0 + float64(k)*(96.0/48.0)
				item.DemandDist[j].Values[k] = baseDemand + float64(k)*10.0
			}
		}

		// Generate salvage table
		item.SalvageTable = make(map[int]map[int]float64)
		for week := 0; week <= 20; week++ {
			item.SalvageTable[week] = make(map[int]float64)
			for inv := 0; inv <= 10000; inv += 100 {
				baseValue := 10.0 * float64(inv)
				weekDecay := math.Pow(0.95, float64(week))
				saturationFactor := math.Min(1.0, float64(inv)/1000.0)
				salvageVal := baseValue * weekDecay * (1 - saturationFactor*0.5)
				item.SalvageTable[week][inv] = math.Max(0, salvageVal)
			}
		}

		items[i] = item
	}

	return items, nil
}

// ===== MAIN =====

func main() {
	fmt.Println(strings.Repeat("=", 80))
	fmt.Println("ECONOMIC ORDERING MODEL (EOM) - Multi-Item Optimization")
	fmt.Println(strings.Repeat("=", 80))

	// Load demo items from configuration
	items, err := LoadDemoItems("config/eom_config.json")
	if err != nil {
		fmt.Printf("Error loading demo items: %v\n", err)
		return
	}

	fmt.Printf("\nLoaded %d items for optimization\n", len(items))
	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("METHOD 1: EOM-TIP (Target Inventory Position - Optimal Profit)")
	fmt.Println(strings.Repeat("=", 80))

	// Solve using EOM-TIP with multiple workers
	tipResults := ProcessItemsConcurrently(items, 4, true, 0)

	fmt.Printf("\n%-15s %-15s %-18s %-18s\n", "ASIN", "Optimal TIP", "Max Profit ($)", "Implied CR")
	fmt.Println(strings.Repeat("-", 70))

	totalProfit := 0.0
	for _, result := range tipResults {
		if result.Error != nil {
			fmt.Printf("%-15s ERROR: %v\n", result.ASIN, result.Error)
		} else {
			fmt.Printf("%-15s %-15.0f %-18.2f %-18.4f\n",
				result.ASIN, result.OptimalTIP, result.MaxProfit, result.CriticalRatio)
			totalProfit += result.MaxProfit
		}
	}
	fmt.Printf("\nTotal Expected Profit (TIP): $%.2f\n", totalProfit)

	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("METHOD 2: EOM-CR (Service Level = 0.85)")
	fmt.Println(strings.Repeat("=", 80))

	// Solve using EOM-CR with same items
	targetServiceLevel := 0.85
	crResults := ProcessItemsConcurrently(items, 4, false, targetServiceLevel)

	fmt.Printf("\n%-15s %-15s %-18s %-18s\n", "ASIN", "Optimal TIP", "Max Profit ($)", "Actual CR")
	fmt.Println(strings.Repeat("-", 70))

	totalProfitCR := 0.0
	for _, result := range crResults {
		if result.Error != nil {
			fmt.Printf("%-15s ERROR: %v\n", result.ASIN, result.Error)
		} else {
			fmt.Printf("%-15s %-15.0f %-18.2f %-18.4f\n",
				result.ASIN, result.OptimalTIP, result.MaxProfit, result.CriticalRatio)
			totalProfitCR += result.MaxProfit
		}
	}
	fmt.Printf("\nTotal Expected Profit (CR): $%.2f\n", totalProfitCR)

	// Detailed analysis for first item
	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("DETAILED ANALYSIS: First Item")
	fmt.Println(strings.Repeat("=", 80))

	item1 := items[0]
	tipResult := tipResults[0]
	crResult := crResults[0]

	fmt.Printf("\nItem: %s\n", item1.ASIN)
	fmt.Printf("Sales Price: $%.2f | Cost: $%.2f | Net Margin: $%.2f\n",
		item1.P, item1.C, item1.P-item1.C)

	cu := item1.CalculateUnderageCost()
	co := item1.CalculateOverageCost()
	fmt.Printf("\nUnderage Cost (cu): $%.2f\n", cu)
	fmt.Printf("Overage Cost (co): $%.2f\n", co)
	fmt.Printf("Critical Ratio (no constraints): %.4f\n", cu/(cu+co))

	fmt.Printf("\n--- TIP Method Results ---\n")
	fmt.Printf("Optimal Inventory Level: %.0f units\n", tipResult.OptimalTIP)
	fmt.Printf("Expected Profit: $%.2f\n", tipResult.MaxProfit)
	fmt.Printf("Implied Service Level (CR): %.4f (%.2f%%)\n", tipResult.CriticalRatio, tipResult.CriticalRatio*100)

	fmt.Printf("\n--- CR Method Results (Target CR=85%%) ---\n")
	fmt.Printf("Optimal Inventory Level: %.0f units\n", crResult.OptimalTIP)
	fmt.Printf("Expected Profit: $%.2f\n", crResult.MaxProfit)
	fmt.Printf("Actual Service Level (CR): %.4f (%.2f%%)\n", crResult.CriticalRatio, crResult.CriticalRatio*100)

	// Sensitivity analysis
	fmt.Printf("\n--- Sensitivity Analysis ---\n")
	fmt.Println("Inventory Level | Profit | Service Level (CR)")
	fmt.Println(strings.Repeat("-", 50))

	for y := tipResult.OptimalTIP - 500; y <= tipResult.OptimalTIP+500; y += 200 {
		if y >= 0 {
			profit := item1.ComputeObjectiveForTIP(y)
			cr := item1.ComputeCumulativeDistributionAtY(y)
			fmt.Printf("%-15.0f | %-6.0f | %-17.4f\n", y, profit, cr)
		}
	}

	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("Recommendation: Use TIP method for maximum profit, CR method for service commitments")
	fmt.Println(strings.Repeat("=", 80))
}

