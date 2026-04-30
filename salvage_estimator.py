"""
Dynamic Salvage Value Estimator with Bayesian Elasticity Learning

Learns price elasticity from observed markdown-demand responses.
Generates salvage tables based on elasticity belief and inventory dynamics.
No elasticity data required at launch — uses category priors, improves over time.
"""

from dataclasses import dataclass, field
from datetime import datetime
from typing import Dict, List, Optional, Tuple
import math


@dataclass
class ElasticityBelief:
    """Bayesian belief over log(-epsilon), where epsilon is price elasticity"""
    asin: str
    mu_log_elasticity: float     # Mean of log(-epsilon)
    tau_log_elasticity: float    # Precision (1/variance); higher = more confident
    n_observations: int          # How many markdown observations incorporated
    last_updated: datetime
    category_prior_mu: float     # Frozen prior for reference


@dataclass
class MarkdownObservation:
    """Observed outcome of a markdown event"""
    asin: str
    week_of_year: int
    markdown_fraction: float     # 0.10 = 10% off
    baseline_demand: float       # Expected demand without markdown
    observed_demand: float       # Actual demand after markdown
    timestamp: datetime


@dataclass
class SalvageTableConfig:
    """Configuration for salvage table generation"""
    max_weeks: int = 20
    max_inventory: int = 10000
    inventory_step: int = 100    # Sample inventory every 100 units
    scrap_fraction: float = 0.05  # Recover 5% of cost by scrapping unsold
    sigma_noise: float = 0.30    # Measurement noise for Bayesian update
    markdown_min: float = 0.05   # Cannot discount less than 5%
    markdown_max: float = 0.50   # Cannot discount more than 50%
    urgency_scale: float = 1.5   # How aggressively to markdown with time


@dataclass
class SalvageEstimate:
    """Output: Salvage value table and elasticity estimate"""
    asin: str
    salvage_table: Dict[int, Dict[int, float]]  # [week][inventory] -> value
    elasticity_point_estimate: float            # -exp(mu_log_elasticity)
    elasticity_confidence: float                # Derived from precision
    computed_at: datetime
    based_on_n_obs: int


# Category-level elasticity priors (from retail economics literature)
CATEGORY_ELASTICITY_PRIORS = {
    "dairy": -0.6,
    "milk": -0.6,
    "eggs": -0.65,
    "butter": -0.70,
    "packaged_food": -0.80,
    "pantry_staples": -0.85,
    "oil": -0.85,
    "flour": -0.80,
    "household_staples": -1.20,
    "paper_towels": -1.20,
    "beverages": -1.00,
    "water": -1.50,  # Highly elastic; substitute easily
    "juice": -0.95,
    "personal_care": -1.50,
    "shampoo": -1.50,
    "toothpaste": -1.40,
    "snacks": -1.80,
    "candy": -2.00,  # Very elastic; discretionary
    "specialty": -2.00,
    "seasonal": -2.50,
    "discretionary": -3.00,
}


class ElasticityLearner:
    """Updates elasticity belief from markdown observations"""

    def __init__(self):
        self.sigma_noise = 0.30

    def initialize_belief(self, asin: str, category: str) -> ElasticityBelief:
        """Create initial belief from category prior"""
        category_lower = category.lower()
        epsilon_prior = CATEGORY_ELASTICITY_PRIORS.get(category_lower, -1.0)

        mu_0 = math.log(-epsilon_prior)  # Log-space to keep positive
        tau_0 = 4.0  # Prior precision: 1/0.5^2 = 4.0 (sigma_0 = 0.5)

        return ElasticityBelief(
            asin=asin,
            mu_log_elasticity=mu_0,
            tau_log_elasticity=tau_0,
            n_observations=0,
            last_updated=datetime.now(),
            category_prior_mu=mu_0,
        )

    def update_belief(self, belief: ElasticityBelief,
                      obs: MarkdownObservation) -> ElasticityBelief:
        """
        Apply Bayesian update using conjugate Gaussian model.

        Demand model: Q = Q_base * (1 - markdown)^(-epsilon)
        Log-linearized: log(Q/Q_base) = -epsilon * log(1 - markdown)

        Conjugate Gaussian update (O(1) time):
        """
        if obs.baseline_demand <= 0 or obs.observed_demand <= 0:
            return belief  # Skip degenerate observations

        delta_log_P = math.log(1.0 - obs.markdown_fraction)  # Negative: price decreased
        delta_log_Q = math.log(obs.observed_demand / obs.baseline_demand)

        # Implied elasticity from this observation
        # delta_log_Q = epsilon * delta_log_P => epsilon = delta_log_Q / delta_log_P
        if abs(delta_log_P) < 1e-6:
            return belief  # Tiny markdown, too noisy

        epsilon_obs_log = delta_log_Q / delta_log_P

        # Likelihood precision contribution
        likelihood_tau = (delta_log_P ** 2) / (self.sigma_noise ** 2)

        # Conjugate update
        tau_new = belief.tau_log_elasticity + likelihood_tau
        mu_new = (
            (belief.tau_log_elasticity * belief.mu_log_elasticity +
             likelihood_tau * epsilon_obs_log) / tau_new
        )

        return ElasticityBelief(
            asin=belief.asin,
            mu_log_elasticity=mu_new,
            tau_log_elasticity=tau_new,
            n_observations=belief.n_observations + 1,
            last_updated=datetime.now(),
            category_prior_mu=belief.category_prior_mu,
        )


class MarkdownAdvisor:
    """Recommends markdown fractions based on inventory position and elasticity"""

    def recommend_markdown(
        self,
        inv_on_hand: float,
        forecast_weekly_demand: float,
        weeks_remaining: float,
        elasticity_estimate: float,
        price: float,
        cost: float,
        config: SalvageTableConfig,
    ) -> float:
        """
        Recommend markdown fraction to sell through remaining inventory.

        Demand model: Q(m) = Q_base * (1 - m)^(-epsilon)

        Goal: Choose m to sell inv_on_hand units in weeks_remaining weeks
        at the current elasticity estimate, subject to [m_min, m_max].
        """
        if forecast_weekly_demand <= 0 or weeks_remaining <= 0:
            return 0.0

        # How many weeks of demand do we need to cover inventory?
        required_weekly_rate = inv_on_hand / weeks_remaining

        if required_weekly_rate <= forecast_weekly_demand:
            # Base demand is sufficient; no markdown needed
            return 0.0

        # Required demand lift
        demand_lift_needed = required_weekly_rate / forecast_weekly_demand

        # Solve: demand_lift_needed = (1 - m)^(-epsilon)
        # => 1 - m = demand_lift_needed^(1/epsilon)
        # => m = 1 - demand_lift_needed^(1/epsilon)

        # Elasticity is negative; 1/epsilon is negative
        # demand_lift_needed^(1/epsilon) = exp((1/epsilon) * log(demand_lift_needed))
        exponent = 1.0 / elasticity_estimate  # Negative
        m_star = 1.0 - math.exp(exponent * math.log(demand_lift_needed))
        m_star = max(m_star, 0.0)  # Ensure non-negative

        # Urgency adjustment: as time runs short, increase markdown aggressiveness
        time_ratio = weeks_remaining / max(config.max_weeks, 1)  # [0, 1]
        urgency = 1.0 + ((1.0 - time_ratio) ** 2) * config.urgency_scale
        m_adjusted = m_star * urgency

        # Feasibility constraints
        m_cost_floor = 1.0 - cost / price if price > 0 else config.markdown_max
        m_final = max(config.markdown_min, min(m_adjusted, config.markdown_max, m_cost_floor))

        return m_final


class SalvageGenerator:
    """Generates 2D salvage value tables from elasticity estimates"""

    def __init__(self, config: Optional[SalvageTableConfig] = None):
        self.config = config or SalvageTableConfig()
        self.markdown_advisor = MarkdownAdvisor()

    def generate_salvage_table(
        self,
        asin: str,
        price: float,
        cost: float,
        demand_mean_weekly: float,
        belief: ElasticityBelief,
    ) -> SalvageEstimate:
        """
        Generate 2D salvage value table by simulating sell-down paths.

        For each (week, inventory_level) pair, simulates forward from that state:
        - Each week, decide markdown fraction based on current inventory and time-to-deadline
        - Estimate demand at that markdown (using elasticity model)
        - Sell and remove from inventory
        - Repeat for remaining weeks
        - Scrap any unsold units at scrap value

        Output table format matches existing EOM interface: Dict[int, Dict[int, float]]
        """
        # Elasticity estimate (always negative)
        elasticity = -math.exp(belief.mu_log_elasticity)
        confidence = math.sqrt(belief.tau_log_elasticity / 4.0)  # Std dev approx

        table: Dict[int, Dict[int, float]] = {}

        for week in range(self.config.max_weeks + 1):
            table[week] = {}

            for inv_level in range(0, self.config.max_inventory + 1, self.config.inventory_step):
                if inv_level == 0:
                    table[week][inv_level] = 0.0
                    continue

                # Simulate sell-down from this week with this inventory
                inv_remaining = float(inv_level)
                total_revenue = 0.0

                # Simulate forward week-by-week until season end
                for w in range(week, -1, -1):
                    if inv_remaining <= 1e-6:
                        break

                    weeks_until_deadline = w + 1
                    m = self.markdown_advisor.recommend_markdown(
                        inv_remaining,
                        demand_mean_weekly,
                        weeks_until_deadline,
                        elasticity,
                        price,
                        cost,
                        self.config,
                    )

                    # Demand at this markdown
                    demand_at_markdown = demand_mean_weekly * ((1.0 - m) ** (-elasticity))

                    # Units sold
                    units_sold = min(inv_remaining, demand_at_markdown)
                    revenue = units_sold * price * (1.0 - m)
                    total_revenue += revenue
                    inv_remaining -= units_sold

                # Scrap unsold inventory
                scrap_value = inv_remaining * cost * self.config.scrap_fraction
                table[week][inv_level] = total_revenue + scrap_value

        return SalvageEstimate(
            asin=asin,
            salvage_table=table,
            elasticity_point_estimate=elasticity,
            elasticity_confidence=confidence,
            computed_at=datetime.now(),
            based_on_n_obs=belief.n_observations,
        )


def demo():
    """Demonstrate elasticity learning and salvage table generation"""

    print("\n" + "=" * 100)
    print("ELASTICITY LEARNING DEMO")
    print("=" * 100)

    learner = ElasticityLearner()

    # Start with dairy prior (epsilon ~ -0.6)
    belief = learner.initialize_belief("ASIN-001-MILK", "Dairy")
    print(f"\nInitial belief for Dairy item:")
    print(f"  Category prior: epsilon = -0.6")
    print(f"  mu_log_epsilon = {belief.mu_log_elasticity:.4f}")
    print(f"  precision tau = {belief.tau_log_elasticity:.2f}")
    print(f"  Estimated epsilon: {-math.exp(belief.mu_log_elasticity):.2f}")

    # Simulate 10 markdown observations
    print(f"\n{'-' * 100}")
    print("Simulated markdown observations (true epsilon = -1.0):")
    print(f"{'-' * 100}")
    print(f"{'Obs':<4} {'Markdown':<12} {'Baseline':<12} {'Observed':<12} {'Updated ε':<12} {'Confidence':<12}")
    print(f"{'-' * 100}")

    observations = [
        (0.10, 100.0, 105.0),  # 10% off, demand went from 100 to 105 (modest lift)
        (0.15, 100.0, 110.0),  # 15% off, demand 110
        (0.10, 100.0, 108.0),  # 10% off, demand 108
        (0.20, 100.0, 118.0),  # 20% off, demand 118
        (0.25, 100.0, 125.0),  # 25% off, demand 125
        (0.15, 100.0, 112.0),  # 15% off, demand 112
        (0.20, 100.0, 122.0),  # 20% off, demand 122
        (0.10, 100.0, 107.0),  # 10% off, demand 107
        (0.25, 100.0, 128.0),  # 25% off, demand 128
        (0.30, 100.0, 135.0),  # 30% off, demand 135
    ]

    for i, (markdown, baseline, observed) in enumerate(observations, 1):
        obs = MarkdownObservation(
            asin="ASIN-001-MILK",
            week_of_year=i,
            markdown_fraction=markdown,
            baseline_demand=baseline,
            observed_demand=observed,
            timestamp=datetime.now(),
        )
        belief = learner.update_belief(belief, obs)
        epsilon_est = -math.exp(belief.mu_log_elasticity)
        confidence = 1.0 / math.sqrt(belief.tau_log_elasticity)

        print(f"{i:<4} {markdown:>10.0%}  {baseline:>10.0f}  {observed:>10.0f}  "
              f"{epsilon_est:>10.2f}  {confidence:>10.3f}")

    print(f"\n{'=' * 100}")
    print(f"Final Bayesian estimate after {belief.n_observations} observations:")
    print(f"  Learned elasticity: {-math.exp(belief.mu_log_elasticity):.2f}")
    print(f"  True elasticity: -1.00")
    print(f"  Standard error: {1.0 / math.sqrt(belief.tau_log_elasticity):.4f}")
    print(f"  Convergence: {'✓ Good agreement' if abs(-math.exp(belief.mu_log_elasticity) + 1.0) < 0.05 else '✗ Still learning'}")

    # Generate salvage table
    print(f"\n{'=' * 100}")
    print("SALVAGE TABLE GENERATION")
    print(f"{'=' * 100}")

    generator = SalvageGenerator()
    salvage_est = generator.generate_salvage_table(
        asin="ASIN-001-MILK",
        price=3.50,
        cost=1.50,
        demand_mean_weekly=100.0,
        belief=belief,
    )

    print(f"\nGenerated salvage table for milk item:")
    print(f"  Price: ${3.50:.2f}, Cost: ${1.50:.2f}, Weekly demand: 100 units")
    print(f"  Elasticity estimate: {salvage_est.elasticity_point_estimate:.2f}")
    print(f"  Based on {salvage_est.based_on_n_obs} observations")

    # Show sample entries from table
    print(f"\nSample salvage values (recovery from unsold inventory):")
    print(f"  {'-' * 70}")
    print(f"  {'Week':<8} {'Inventory Level':<20} {'Salvage Value':<15}")
    print(f"  {'-' * 70}")

    for week in [0, 5, 10, 15, 20]:
        if week in salvage_est.salvage_table:
            for inv_level in [100, 500, 1000, 2000]:
                if inv_level in salvage_est.salvage_table[week]:
                    value = salvage_est.salvage_table[week][inv_level]
                    print(f"  {week:<8} {inv_level:<20} ${value:>13.2f}")

    return belief, salvage_est


if __name__ == "__main__":
    demo()
