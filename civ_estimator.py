"""
Dynamic Customer In-Stock Value (CIV / Lambda) Estimator

Estimates how much basket value is lost when an item is out of stock.
No historical transaction data required — uses theoretical estimation from:
  - Velocity (how often item is bought)
  - Demand Stability (planned vs impulse purchase)
  - Category Essentiality (expert prior on basket centrality)
"""

from dataclasses import dataclass, field
from datetime import datetime
from typing import Dict, List, Optional
import statistics
import math


@dataclass
class ItemCatalogFeatures:
    """Input: Item characteristics from catalog"""
    asin: str
    category: str
    subcategory: str
    weekly_velocity_units: float  # Mean units sold per week
    demand_cv: Optional[float]    # Coefficient of variation; None if unknown
    price: float


@dataclass
class CIVEstimate:
    """Output: Estimated lambda_ value for Item"""
    asin: str
    lambda_value: float           # The computed CIV ($)
    civ_score: float             # Normalized [0,1] composite before scaling
    velocity_score: float
    stability_score: float
    essentiality_score: float
    confidence: float            # [0,1]; 0 = used fallback prior
    computed_at: datetime
    data_version: str = "v1"


@dataclass
class CIVConfig:
    """Configuration for CIV computation"""
    lambda_min: float = 0.10
    lambda_max: float = 3.00
    weight_velocity: float = 0.35
    weight_stability: float = 0.25
    weight_essentiality: float = 0.40
    fallback_lambda: float = 0.87
    category_essentiality: Dict[str, float] = field(default_factory=lambda: {
        "dairy": 1.00,
        "milk": 1.00,
        "eggs": 0.95,
        "butter": 0.90,
        "household_staples": 0.85,
        "paper_towels": 0.85,
        "laundry": 0.85,
        "pantry_staples": 0.80,
        "oil": 0.80,
        "sugar": 0.78,
        "flour": 0.78,
        "beverages": 0.70,
        "water": 0.70,
        "juice": 0.68,
        "personal_care": 0.65,
        "shampoo": 0.65,
        "toothpaste": 0.65,
        "snacks": 0.45,
        "candy": 0.40,
        "specialty": 0.35,
        "ethnic": 0.35,
        "seasonal": 0.20,
        "discretionary": 0.20,
    })

    # Category-level demand CV priors (when item-level CV unknown)
    category_stability_priors: Dict[str, float] = field(default_factory=lambda: {
        "dairy": 0.08,              # Low CV = stable destination item
        "milk": 0.08,
        "eggs": 0.10,
        "household_staples": 0.12,
        "pantry_staples": 0.15,
        "beverages": 0.18,
        "personal_care": 0.20,
        "snacks": 0.35,             # High CV = impulse purchases
        "seasonal": 0.40,
        "discretionary": 0.45,
    })


class CIVEstimator:
    """Estimates customer in-stock value (lambda_) for inventory items"""

    def __init__(self, config: Optional[CIVConfig] = None):
        self.config = config or CIVConfig()

    def compute_civ(self, features: ItemCatalogFeatures,
                    category_p90_velocity: Optional[Dict[str, float]] = None) -> CIVEstimate:
        """
        Compute CIV estimate for a single item.

        Args:
            features: Item catalog characteristics
            category_p90_velocity: Dict mapping category -> P90 velocity for normalization.
                                  If None, uses a default global P90.

        Returns:
            CIVEstimate with lambda_value and component scores
        """
        if category_p90_velocity is None:
            category_p90_velocity = {}

        # --- Velocity Score: normalized by category P90 ---
        p90_vel = category_p90_velocity.get(features.category.lower(), 50.0)
        if p90_vel > 0:
            velocity_score = min(features.weekly_velocity_units / p90_vel, 1.0)
        else:
            velocity_score = 0.5  # Prior if no category stats
        velocity_score = max(velocity_score, 0.0)

        # --- Stability Score: inverse of demand CV ---
        if features.demand_cv is not None and features.demand_cv >= 0:
            stability_score = 1.0 / (1.0 + features.demand_cv)
        else:
            # Use category-level prior
            category_lower = features.category.lower()
            prior_cv = self.config.category_stability_priors.get(category_lower, 0.25)
            stability_score = 1.0 / (1.0 + prior_cv)

        # --- Essentiality Score: category lookup ---
        category_lower = features.category.lower()
        essentiality_score = self.config.category_essentiality.get(category_lower, 0.45)

        # --- Confidence: how many of the 3 inputs are known ---
        n_known = 0
        if features.weekly_velocity_units > 0:
            n_known += 1
        if features.demand_cv is not None and features.demand_cv >= 0:
            n_known += 1
        if category_lower in self.config.category_essentiality:
            n_known += 1

        confidence = n_known / 3.0

        # --- Composite CIV Score ---
        if confidence < 0.33:
            # Fully unknown item: use prior
            return CIVEstimate(
                asin=features.asin,
                lambda_value=self.config.fallback_lambda,
                civ_score=0.0,
                velocity_score=0.0,
                stability_score=0.0,
                essentiality_score=0.0,
                confidence=0.0,
                computed_at=datetime.now(),
            )

        civ_score = (
            self.config.weight_velocity * velocity_score +
            self.config.weight_stability * stability_score +
            self.config.weight_essentiality * essentiality_score
        )

        # --- Scale to lambda_ range ---
        lambda_value = (
            self.config.lambda_min +
            civ_score * (self.config.lambda_max - self.config.lambda_min)
        )

        return CIVEstimate(
            asin=features.asin,
            lambda_value=lambda_value,
            civ_score=civ_score,
            velocity_score=velocity_score,
            stability_score=stability_score,
            essentiality_score=essentiality_score,
            confidence=confidence,
            computed_at=datetime.now(),
        )

    def batch_compute_civ(self, features_list: List[ItemCatalogFeatures]) -> Dict[str, CIVEstimate]:
        """
        Compute CIV for a batch of items.

        Pre-computes P90 velocity by category for normalization, then processes all items.
        """
        # Compute category-level P90 velocity
        category_velocities: Dict[str, List[float]] = {}
        for feat in features_list:
            cat = feat.category.lower()
            if cat not in category_velocities:
                category_velocities[cat] = []
            if feat.weekly_velocity_units > 0:
                category_velocities[cat].append(feat.weekly_velocity_units)

        category_p90: Dict[str, float] = {}
        for cat, velocities in category_velocities.items():
            if velocities:
                # P90: 90th percentile
                sorted_vel = sorted(velocities)
                idx = min(len(sorted_vel) - 1, int(0.90 * len(sorted_vel)))
                category_p90[cat] = sorted_vel[idx]
            else:
                category_p90[cat] = 50.0  # default

        # Compute CIV for each item
        results: Dict[str, CIVEstimate] = {}
        for feat in features_list:
            est = self.compute_civ(feat, category_p90)
            results[feat.asin] = est

        return results


def demo():
    """Demonstrate CIV estimation with realistic catalog examples"""

    config = CIVConfig()
    estimator = CIVEstimator(config)

    # Create sample catalog
    catalog = [
        # Destination items (high CIV)
        ItemCatalogFeatures("ASIN-001-MILK", "Dairy", "milk", 120.0, 0.08, 3.50),
        ItemCatalogFeatures("ASIN-002-EGGS", "Dairy", "eggs", 95.0, 0.10, 4.20),
        ItemCatalogFeatures("ASIN-003-BUTTER", "Dairy", "butter", 60.0, 0.09, 5.00),

        # Core staples (medium CIV)
        ItemCatalogFeatures("ASIN-004-BREAD", "Pantry", "bread", 85.0, 0.15, 2.50),
        ItemCatalogFeatures("ASIN-005-OIL", "Pantry", "oil", 40.0, 0.12, 8.00),
        ItemCatalogFeatures("ASIN-006-FLOUR", "Pantry", "flour", 35.0, 0.14, 3.00),

        # Household staples
        ItemCatalogFeatures("ASIN-007-TOWELS", "Household", "paper_towels", 70.0, 0.20, 12.00),

        # Low velocity fillers (low CIV)
        ItemCatalogFeatures("ASIN-008-SALT", "Pantry", "salt", 15.0, 0.25, 2.00),
        ItemCatalogFeatures("ASIN-009-SPICE", "Pantry", "spice", 5.0, 0.40, 6.00),
        ItemCatalogFeatures("ASIN-010-CANDY", "Snacks", "candy", 20.0, 0.35, 1.50),
    ]

    # Batch compute
    results = estimator.batch_compute_civ(catalog)

    # Display results
    print("\n" + "=" * 100)
    print("DYNAMIC CIV ESTIMATION - CATALOG ANALYSIS")
    print("=" * 100)
    print(f"{'ASIN':<20} {'Category':<15} {'Velocity':<10} {'λ (CIV)':<10} {'Score':<8} {'Confidence':<12}")
    print("-" * 100)

    for asin in sorted(results.keys()):
        est = results[asin]
        feat = next(f for f in catalog if f.asin == asin)

        print(f"{asin:<20} {feat.category:<15} {feat.weekly_velocity_units:>8.0f} "
              f"${est.lambda_value:>7.2f}   {est.civ_score:>6.2f}  {est.confidence:>6.0%}")

    print("\n" + "=" * 100)
    print("INTERPRETATION")
    print("=" * 100)
    print("λ (CIV) Column:")
    print("  - High λ (~$2.50+): Destination item; OOS loses entire basket")
    print("  - Medium λ (~$0.85): Core item; OOS loses some complementary purchases")
    print("  - Low λ (~$0.20): Filler item; OOS has minimal basket impact")
    print("\nConfidence Column:")
    print("  - 100%: All 3 inputs known (velocity, demand CV, category)")
    print("  - 67%: 2 of 3 known")
    print("  - 33%: 1 of 3 known (uses fallback prior)")
    print("  - 0%: Unknown (uses fallback λ = $0.87)")

    return results


if __name__ == "__main__":
    demo()
