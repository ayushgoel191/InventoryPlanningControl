"""
Economic Ordering Model (EOM) Implementation
Two methods: EOM-CR (Service Level) and EOM-TIP (Optimal Profit)

Based on the research document by Alvaro Maggiar on EOM for inventory planning.
Supports processing millions of SKUs at scale.
"""

import numpy as np
from typing import List, Dict, Tuple, Optional
from dataclasses import dataclass
import math
from concurrent.futures import ThreadPoolExecutor, ProcessPoolExecutor
from functools import lru_cache
import time


@dataclass
class Distribution:
    """Represents a probability distribution with quantiles"""
    quantiles: np.ndarray  # 50 percentile points
    values: np.ndarray     # corresponding values


@dataclass
class EOAResult:
    """Result of EOM optimization for a single item"""
    asin: str
    optimal_tip: float
    max_profit: float
    critical_ratio: float
    target_service_level: Optional[float] = None
    error: Optional[str] = None


class Item:
    """Represents an SKU with all EOM parameters"""

    def __init__(self, asin: str):
        self.asin = asin

        # Pricing parameters
        self.p = 0.0          # Sales price
        self.p_prime = 0.0    # Additional CP terms on sale
        self.k = 0.0          # Penalty of lost sale
        self.c = 0.0          # Purchasing cost
        self.c_prime = 0.0    # Additional CP terms on receipt
        self.a = 0.0          # Cost of arrival

        # Physical parameters
        self.v = 0.0          # Volume of item
        self.lambda_ = 0.0    # Consumer in-stock value (CIV)
        self.alpha = 0.0      # CIV scaling factor
        self.h = 0.0          # Yearly cost of capital

        # Penalty parameters
        self.h_bar = 0.0      # Per unit penalty
        self.h_prime = 0.0    # Per volume penalty
        self.h_hat = 0.0      # Per value unit penalty

        # Distributions
        self.vlt_dist: Optional[Distribution] = None  # Lead time
        self.demand_dist: List[Optional[Distribution]] = []  # Demand for each VLT

        # Salvage table: {week: {inventory_level: salvage_value}}
        self.salvage_table: Dict[int, Dict[int, float]] = {}

        # Review period in days
        self.review_period = 7
        self.current_inventory = 0.0

    def calculate_underage_cost(self) -> float:
        """cu = p + p' + k - (c - c') + α*Λ - a"""
        return self.p + self.p_prime + self.k - (self.c - self.c_prime) + \
               self.alpha * self.lambda_ - self.a

    def calculate_overage_cost(self) -> float:
        """co = (c - c') + a + h̄ + h'*v + ĥ*(c - c')"""
        net_cost = self.c - self.c_prime
        return net_cost + self.a + self.h_bar + self.h_prime * self.v + \
               self.h_hat * net_cost

    def calculate_holding_cost(self, lead_days: int) -> float:
        """hl = (γ^l - 1) where γ = 1 + h/365"""
        gamma = 1.0 + self.h / 365.0
        return math.pow(gamma, lead_days) - 1.0

    def get_salvage_value(self, lead_time_days: int, leftover_inventory: float) -> float:
        """Get salvage value using bilinear interpolation"""
        if leftover_inventory <= 0:
            return 0.0

        weeks = lead_time_days / 7.0
        week1 = int(weeks)
        week2 = week1 + 1
        frac_week = weeks - week1

        inv_level1 = int(leftover_inventory)
        inv_level2 = inv_level1 + 1
        frac_inv = leftover_inventory - inv_level1

        # Get values from salvage table with bounds checking
        val11 = self._get_salvage_table_value(week1, inv_level1)
        val12 = self._get_salvage_table_value(week1, inv_level2)
        val21 = self._get_salvage_table_value(week2, inv_level1)
        val22 = self._get_salvage_table_value(week2, inv_level2)

        # Bilinear interpolation
        val1 = val11 * (1 - frac_inv) + val12 * frac_inv
        val2 = val21 * (1 - frac_inv) + val22 * frac_inv
        result = val1 * (1 - frac_week) + val2 * frac_week

        return max(0, result)

    def _get_salvage_table_value(self, week: int, inv_level: int) -> float:
        """Safely get salvage table value with bounds checking"""
        if week in self.salvage_table:
            week_map = self.salvage_table[week]
            if inv_level in week_map:
                return week_map[inv_level]
            # Return closest available
            for i in range(inv_level, -1, -1):
                if i in week_map:
                    return week_map[i]
        return 0.0

    def compute_objective_for_tip(self, target_y: float) -> float:
        """Compute z(y) for TIP method"""
        cu = self.calculate_underage_cost()
        co = self.calculate_overage_cost()
        net_cost = self.c - self.c_prime

        total_profit = 0.0
        count = 0.0

        if self.vlt_dist is None or not self.demand_dist:
            return 0.0

        # Iterate over VLT and demand quantiles
        for vlt_idx, vlt_val in enumerate(self.vlt_dist.values):
            lead_days = int(vlt_val)
            holding_cost = self.calculate_holding_cost(lead_days)

            if vlt_idx < len(self.demand_dist) and self.demand_dist[vlt_idx] is not None:
                for demand_val in self.demand_dist[vlt_idx].values:
                    demand = demand_val

                    # Expected revenue
                    expected_revenue = cu * demand

                    # Underage cost
                    underage_term = cu * max(0, demand - target_y)

                    # Overage cost and salvage
                    leftover = max(0, target_y - demand)
                    overage_cost_linear = (co + holding_cost * net_cost) * leftover
                    salvage_value = self.get_salvage_value(lead_days, leftover)

                    # Profit for this realization
                    profit = expected_revenue - underage_term - overage_cost_linear + salvage_value
                    total_profit += profit
                    count += 1.0

        return total_profit / count if count > 0 else 0.0

    def compute_gradient_for_tip(self, target_y: float) -> float:
        """Compute dz/dy for TIP method"""
        cu = self.calculate_underage_cost()
        co = self.calculate_overage_cost()
        net_cost = self.c - self.c_prime

        total_gradient = 0.0
        count = 0.0

        if self.vlt_dist is None or not self.demand_dist:
            return 0.0

        for vlt_idx, vlt_val in enumerate(self.vlt_dist.values):
            lead_days = int(vlt_val)
            holding_cost = self.calculate_holding_cost(lead_days)

            if vlt_idx < len(self.demand_dist) and self.demand_dist[vlt_idx] is not None:
                for demand_val in self.demand_dist[vlt_idx].values:
                    demand = demand_val

                    prob_demand_greater = 1.0 if demand > target_y else 0.0
                    prob_demand_less = 1.0 - prob_demand_greater

                    leftover = max(0, target_y - demand)

                    # Approximate marginal salvage value
                    delta_salvage = 0.0
                    if leftover > 0:
                        sv1 = self.get_salvage_value(lead_days, leftover)
                        sv2 = self.get_salvage_value(lead_days, leftover + 1)
                        delta_salvage = sv2 - sv1

                    gradient = -cu * prob_demand_greater + \
                              (co + holding_cost * net_cost) * prob_demand_less - delta_salvage
                    total_gradient += gradient
                    count += 1.0

        return total_gradient / count if count > 0 else 0.0

    def compute_cumulative_distribution_at_y(self, y: float) -> float:
        """Compute H(y) = E_L[F_L(y)] for CR method"""
        total_prob = 0.0
        count = 0.0

        if self.vlt_dist is None or not self.demand_dist:
            return 0.0

        for vlt_idx in range(len(self.vlt_dist.values)):
            if vlt_idx < len(self.demand_dist) and self.demand_dist[vlt_idx] is not None:
                demand_less_or_equal = sum(1 for d in self.demand_dist[vlt_idx].values if d <= y)
                total_demands = len(self.demand_dist[vlt_idx].values)

                if total_demands > 0:
                    fl = demand_less_or_equal / total_demands
                    total_prob += fl
                    count += 1.0

        return total_prob / count if count > 0 else 0.0


class EOMActor:
    """EOM solver using bisection methods"""

    def __init__(self, max_iterations: int = 100, tolerance: float = 1.0):
        self.max_iterations = max_iterations
        self.tolerance = tolerance

    def solve_eom_cr(self, item: Item, target_service_level: float, verify_integer: bool = True, verify_range: int = 5) -> EOAResult:
        """Solve CR method: find y where H(y) >= target_service_level

        Args:
            item: The item to optimize
            target_service_level: Target service level (e.g., 0.85)
            verify_integer: If True, verify solution at nearby integers
            verify_range: Check ±this many integers around optimal (default 5)
        """
        result = EOAResult(
            asin=item.asin,
            optimal_tip=0.0,
            max_profit=0.0,
            critical_ratio=0.0,
            target_service_level=target_service_level
        )

        # Bisection search
        left = 0.0
        right = 10000.0

        # Find right bound where H(right) >= target_service_level
        for _ in range(20):
            if item.compute_cumulative_distribution_at_y(right) >= target_service_level:
                break
            right *= 2

        for _ in range(self.max_iterations):
            mid = (left + right) / 2.0
            h = item.compute_cumulative_distribution_at_y(mid)

            if abs(h - target_service_level) < 0.0001:
                result.optimal_tip = mid
                result.critical_ratio = h
                result.max_profit = item.compute_objective_for_tip(mid)

                # Verify at nearby integers if requested
                if verify_integer:
                    result = self._verify_cr_integer_optimality(item, result, target_service_level, verify_range)

                return result

            if h < target_service_level:
                left = mid
            else:
                right = mid

        result.optimal_tip = (left + right) / 2.0
        result.critical_ratio = item.compute_cumulative_distribution_at_y(result.optimal_tip)
        result.max_profit = item.compute_objective_for_tip(result.optimal_tip)

        # Verify at nearby integers if requested
        if verify_integer:
            result = self._verify_cr_integer_optimality(item, result, target_service_level, verify_range)

        return result

    def _verify_cr_integer_optimality(self, item: Item, result: EOAResult, target_service_level: float, verify_range: int = 5) -> EOAResult:
        """Verify CR solution by checking nearby integers

        For CR method, we want the minimum y where H(y) >= target.

        Args:
            item: The item
            result: Current result with continuous optimal TIP
            target_service_level: Target service level
            verify_range: Check ±verify_range integers

        Returns:
            Updated result with integer-verified TIP
        """
        # Round to nearest integer
        y_center = int(round(result.optimal_tip))

        # For CR, find the MINIMUM y that meets service level
        best_y = y_center
        best_sl = item.compute_cumulative_distribution_at_y(y_center)

        # Check range: start from lower bound and find minimum that meets target
        found_valid = best_sl >= target_service_level
        candidates = []

        for y in range(max(0, y_center - verify_range), y_center + verify_range + 1):
            sl = item.compute_cumulative_distribution_at_y(y)
            if sl >= target_service_level:
                candidates.append((y, sl))

        if candidates:
            # Pick minimum y that meets service level
            best_y, best_sl = min(candidates, key=lambda x: x[0])

        # Update result with integer optimum
        result.optimal_tip = float(best_y)
        result.max_profit = item.compute_objective_for_tip(best_y)
        result.critical_ratio = item.compute_cumulative_distribution_at_y(best_y)

        return result

    def solve_eom_tip(self, item: Item, verify_integer: bool = True, verify_range: int = 5) -> EOAResult:
        """Solve TIP method: find y that minimizes cost (maximizes profit)

        Args:
            item: The item to optimize
            verify_integer: If True, verify solution at nearby integers
            verify_range: Check ±this many integers around optimal (default 5)
        """
        result = EOAResult(
            asin=item.asin,
            optimal_tip=0.0,
            max_profit=0.0,
            critical_ratio=0.0
        )

        # Bisection on gradient: find where dz/dy = 0
        left = 0.0
        right = 10000.0

        grad_left = item.compute_gradient_for_tip(left)
        grad_right = item.compute_gradient_for_tip(right)

        # Expand right if needed
        while grad_right < 0 and right < 100000:
            right *= 2
            grad_right = item.compute_gradient_for_tip(right)

        for _ in range(self.max_iterations):
            mid = (left + right) / 2.0
            grad = item.compute_gradient_for_tip(mid)

            if abs(grad) < self.tolerance:
                result.optimal_tip = mid
                result.max_profit = item.compute_objective_for_tip(mid)
                result.critical_ratio = item.compute_cumulative_distribution_at_y(mid)

                # Verify at nearby integers if requested
                if verify_integer:
                    result = self._verify_integer_optimality(item, result, verify_range)

                return result

            if grad < 0:
                left = mid
            else:
                right = mid

        result.optimal_tip = (left + right) / 2.0
        result.max_profit = item.compute_objective_for_tip(result.optimal_tip)
        result.critical_ratio = item.compute_cumulative_distribution_at_y(result.optimal_tip)

        # Verify at nearby integers if requested
        if verify_integer:
            result = self._verify_integer_optimality(item, result, verify_range)

        return result

    def _verify_integer_optimality(self, item: Item, result: EOAResult, verify_range: int = 5) -> EOAResult:
        """Verify solution by checking nearby integers

        Args:
            item: The item
            result: Current result with continuous optimal TIP
            verify_range: Check ±verify_range integers

        Returns:
            Updated result with integer-verified TIP
        """
        # Round to nearest integer
        y_center = int(round(result.optimal_tip))

        # Check range of integers
        best_y = y_center
        best_profit = item.compute_objective_for_tip(y_center)

        # Check nearby integers
        for y in range(max(0, y_center - verify_range), y_center + verify_range + 1):
            if y == y_center:
                continue  # Already evaluated

            profit = item.compute_objective_for_tip(y)
            if profit > best_profit:
                best_profit = profit
                best_y = y

        # Update result with integer optimum
        result.optimal_tip = float(best_y)
        result.max_profit = item.compute_objective_for_tip(best_y)
        result.critical_ratio = item.compute_cumulative_distribution_at_y(best_y)

        return result


# ===== DUMMY DATA GENERATION =====

def normal_inverse(p: float) -> float:
    """Approximate inverse normal CDF (simplified)"""
    if p <= 0 or p >= 1:
        return 0.0

    # Abramowitz and Stegun approximation
    if p < 0.5:
        return -normal_inverse(1 - p)

    # For p >= 0.5
    a1 = -3.969683028665376e+01
    a2 = 2.221222899801429e+02
    a3 = -2.821152023902548e+02
    a4 = 1.340426573691379e+02
    a5 = -2.402303233503123e+01

    b1 = -5.447609879822406e+01
    b2 = 1.615858368580409e+02
    b3 = -1.556989798598866e+02
    b4 = 6.680131188771972e+01

    c1 = -7.784894002430293e-03
    c2 = -3.223671290700182e-01
    c3 = -2.400758277161838e+00
    c4 = -2.549732539343734e+00

    d1 = 7.784695709041462e-03
    d2 = 3.224671290700182e-01
    d3 = 2.445134137142996e+00

    if p < 0.02425:
        q = math.sqrt(-2.0 * math.log(p))
        return -((((c1*q+c2)*q+c3)*q + c4) / ((((d1*q+d2)*q+d3)*q + 1)))

    if p <= 0.97575:
        q = p - 0.5
        r = q * q
        return (((((a1*r+a2)*r+a3)*r+a4)*r+a5)*q) / (((((b1*r+b2)*r+b3)*r+b4)*r + 1))

    q = math.sqrt(-2.0 * math.log(1-p))
    return (((((c1*q+c2)*q+c3)*q + c4) / ((((d1*q+d2)*q+d3)*q + 1))))


def generate_dummy_distribution(mean: float, std_dev: float, num_quantiles: int = 50) -> Distribution:
    """Generate synthetic distribution data"""
    quantiles = np.zeros(num_quantiles)
    values = np.zeros(num_quantiles)

    for i in range(num_quantiles):
        if i < num_quantiles - 1:
            percentile = 2.0 + (i * 96.0 / (num_quantiles - 2))
            quantiles[i] = percentile
        else:
            quantiles[i] = 100.0

        z = normal_inverse(quantiles[i] / 100.0)
        values[i] = mean + z * std_dev
        if values[i] < 0:
            values[i] = 0.0

    return Distribution(quantiles=quantiles, values=values)


def generate_dummy_salvage_table(max_weeks: int = 20, max_inventory: int = 10000) -> Dict[int, Dict[int, float]]:
    """Generate synthetic salvage value data"""
    table = {}

    for week in range(max_weeks + 1):
        table[week] = {}
        for inv in range(0, max_inventory + 1, 100):
            base_value = 10.0 * inv
            week_decay = math.pow(0.95, week)
            saturation_factor = min(1.0, inv / 1000.0)

            salvage_val = base_value * week_decay * (1 - saturation_factor * 0.5)
            table[week][inv] = max(0, salvage_val)

    return table


def generate_dummy_item(asin: str, seed: int = 0) -> Item:
    """Create a sample item for testing"""
    np.random.seed(seed)

    item = Item(asin)
    item.p = 19.99
    item.p_prime = -3.77
    item.k = 4.0
    item.c = 14.99
    item.c_prime = 2.13
    item.a = 0.0
    item.v = 0.0635
    item.lambda_ = 0.87
    item.alpha = 1.0
    item.h = 0.08
    item.h_bar = 0.015
    item.h_prime = 0.0
    item.h_hat = 1.0
    item.review_period = 7
    item.current_inventory = 500.0

    # VLT distribution (mean 12 days, std 5 days)
    item.vlt_dist = generate_dummy_distribution(12, 5, 50)

    # Demand distributions
    item.demand_dist = []
    for i in range(50):
        vlt_days = item.vlt_dist.values[i]
        demand_mean = 500.0 * (vlt_days / 7.0)
        demand_std = 100.0 * math.sqrt(vlt_days / 7.0)
        item.demand_dist.append(generate_dummy_distribution(demand_mean, demand_std, 50))

    # Salvage table
    item.salvage_table = generate_dummy_salvage_table(20, 10000)

    return item


def process_items_concurrently(items: List[Item], num_workers: int,
                               use_tip: bool = True, service_level: float = 0.85) -> List[EOAResult]:
    """Process multiple items in parallel"""
    eom = EOMActor(max_iterations=100, tolerance=1.0)
    results = []

    def process_item(item):
        if use_tip:
            return eom.solve_eom_tip(item)
        else:
            return eom.solve_eom_cr(item, service_level)

    with ThreadPoolExecutor(max_workers=num_workers) as executor:
        results = list(executor.map(process_item, items))

    return results


# ===== MAIN =====

def main():
    print("=" * 80)
    print("ECONOMIC ORDERING MODEL (EOM) - Multi-Item Optimization")
    print("=" * 80)

    # Generate dummy items
    num_items = 10
    items = [generate_dummy_item(f"ASIN-{i+1:06d}", seed=i) for i in range(num_items)]

    print(f"\nGenerated {num_items} items for optimization\n")

    # ===== METHOD 1: EOM-TIP =====
    print("\n" + "=" * 80)
    print("METHOD 1: EOM-TIP (Target Inventory Position - Optimal Profit)")
    print("=" * 80)

    start_time = time.time()
    tip_results = process_items_concurrently(items, num_workers=4, use_tip=True)
    tip_time = time.time() - start_time

    print(f"\n{'ASIN':<15} {'Optimal TIP':<15} {'Max Profit ($)':<18} {'Implied CR':<15}")
    print("-" * 70)

    total_profit_tip = 0.0
    for result in tip_results:
        if result.error:
            print(f"{result.asin:<15} ERROR: {result.error}")
        else:
            print(f"{result.asin:<15} {result.optimal_tip:<15.0f} {result.max_profit:<18.2f} {result.critical_ratio:<15.4f}")
            total_profit_tip += result.max_profit

    print(f"\nTotal Expected Profit (TIP): ${total_profit_tip:.2f}")
    print(f"Processing time: {tip_time:.3f}s")

    # ===== METHOD 2: EOM-CR =====
    print("\n" + "=" * 80)
    print("METHOD 2: EOM-CR (Service Level = 0.85)")
    print("=" * 80)

    start_time = time.time()
    target_service_level = 0.85
    cr_results = process_items_concurrently(items, num_workers=4, use_tip=False,
                                            service_level=target_service_level)
    cr_time = time.time() - start_time

    print(f"\n{'ASIN':<15} {'Optimal TIP':<15} {'Max Profit ($)':<18} {'Actual CR':<15}")
    print("-" * 70)

    total_profit_cr = 0.0
    for result in cr_results:
        if result.error:
            print(f"{result.asin:<15} ERROR: {result.error}")
        else:
            print(f"{result.asin:<15} {result.optimal_tip:<15.0f} {result.max_profit:<18.2f} {result.critical_ratio:<15.4f}")
            total_profit_cr += result.max_profit

    print(f"\nTotal Expected Profit (CR): ${total_profit_cr:.2f}")
    print(f"Processing time: {cr_time:.3f}s")

    # ===== DETAILED ANALYSIS =====
    print("\n" + "=" * 80)
    print("DETAILED ANALYSIS: First Item")
    print("=" * 80)

    item1 = items[0]
    tip_result = tip_results[0]
    cr_result = cr_results[0]

    print(f"\nItem: {item1.asin}")
    print(f"Sales Price: ${item1.p:.2f} | Cost: ${item1.c:.2f} | Net Margin: ${item1.p - item1.c:.2f}")

    cu = item1.calculate_underage_cost()
    co = item1.calculate_overage_cost()
    print(f"\nUnderage Cost (cu): ${cu:.2f}")
    print(f"Overage Cost (co): ${co:.2f}")
    print(f"Critical Ratio (no constraints): {cu/(cu+co):.4f}")

    print(f"\n--- TIP Method Results (with Integer Verification) ---")
    print(f"Optimal Inventory Level: {tip_result.optimal_tip:.0f} units")
    print(f"Expected Profit: ${tip_result.max_profit:.2f}")
    print(f"Implied Service Level (CR): {tip_result.critical_ratio:.4f} ({tip_result.critical_ratio*100:.2f}%)")

    print(f"\n--- CR Method Results (Target CR=85%) ---")
    print(f"Optimal Inventory Level: {cr_result.optimal_tip:.0f} units")
    print(f"Expected Profit: ${cr_result.max_profit:.2f}")
    print(f"Actual Service Level (CR): {cr_result.critical_ratio:.4f} ({cr_result.critical_ratio*100:.2f}%)")

    # Sensitivity analysis
    print(f"\n--- Sensitivity Analysis ---")
    print(f"{'Inventory Level':<18} {'Profit':<15} {'Service Level (CR)':<20}")
    print("-" * 55)

    base_y = tip_result.optimal_tip
    for y in np.linspace(base_y - 500, base_y + 500, 6):
        if y >= 0:
            profit = item1.compute_objective_for_tip(y)
            cr = item1.compute_cumulative_distribution_at_y(y)
            # Highlight the optimal integer
            if int(y) == int(tip_result.optimal_tip):
                print(f"{y:<18.0f} {profit:<15.0f} {cr:<20.4f}  ← OPTIMAL")
            else:
                print(f"{y:<18.0f} {profit:<15.0f} {cr:<20.4f}")

    # Integer verification analysis
    print(f"\n--- Integer Verification Analysis ---")
    print(f"Verification checked inventory levels: ±5 integers around optimal")
    print(f"{'Y':<8} {'Profit':<15} {'Service Level':<18} {'Notes':<20}")
    print("-" * 65)

    optimal_y = int(round(tip_result.optimal_tip))
    for y in range(max(0, optimal_y - 5), optimal_y + 6):
        profit = item1.compute_objective_for_tip(y)
        cr = item1.compute_cumulative_distribution_at_y(y)
        if y == int(tip_result.optimal_tip):
            print(f"{y:<8} {profit:<15.0f} {cr:<18.4f} ← SELECTED")
        elif y == optimal_y:
            print(f"{y:<8} {profit:<15.0f} {cr:<18.4f}")
        else:
            print(f"{y:<8} {profit:<15.0f} {cr:<18.4f}")

    print("\n" + "=" * 80)
    print("Recommendation: Use TIP method for maximum profit, CR method for service commitments")
    print("=" * 80)


if __name__ == "__main__":
    main()
