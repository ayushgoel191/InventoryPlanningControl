"""
Item Factory: Assembles Item objects from CIV and Salvage caches.

Acts as the seam between the estimation modules (civ_estimator, salvage_estimator)
and the EOM solver. Handles staleness tracking and fallback strategies.
"""

from dataclasses import dataclass, field
from datetime import datetime, timedelta
from typing import Dict, Optional, List
import json
import os
from eom import Item, Distribution, generate_dummy_item, generate_dummy_salvage_table
from civ_estimator import CIVEstimate, CIVEstimator, ItemCatalogFeatures, CIVConfig
from salvage_estimator import (
    SalvageEstimate,
    ElasticityBelief,
    SalvageGenerator,
    SalvageTableConfig,
    ElasticityLearner,
)


@dataclass
class CacheEntry:
    """Generic cache entry with staleness tracking"""
    data: any
    computed_at: datetime
    ttl_hours: int

    def is_stale(self) -> bool:
        """Check if entry has exceeded TTL"""
        return datetime.now() - self.computed_at > timedelta(hours=self.ttl_hours)


class CIVCache:
    """In-memory cache for CIV estimates with TTL"""

    def __init__(self, ttl_hours: int = 168):  # Default 1 week
        self.ttl_hours = ttl_hours
        self.cache: Dict[str, CacheEntry] = {}
        self.estimator = CIVEstimator(CIVConfig())

    def get(self, asin: str) -> Optional[CIVEstimate]:
        """Get CIV estimate; None if not in cache or stale"""
        if asin not in self.cache:
            return None
        entry = self.cache[asin]
        if entry.is_stale():
            del self.cache[asin]
            return None
        return entry.data

    def set(self, asin: str, estimate: CIVEstimate):
        """Store CIV estimate in cache"""
        self.cache[asin] = CacheEntry(
            data=estimate,
            computed_at=datetime.now(),
            ttl_hours=self.ttl_hours,
        )

    def expedited_compute(self, features: ItemCatalogFeatures) -> Optional[CIVEstimate]:
        """Fast in-memory compute without pre-computed P90 stats"""
        return self.estimator.compute_civ(features)

    def batch_refresh(self, features_list: List[ItemCatalogFeatures]):
        """Batch compute and cache CIV estimates (weekly job)"""
        results = self.estimator.batch_compute_civ(features_list)
        for asin, estimate in results.items():
            self.set(asin, estimate)


class SalvageCache:
    """In-memory cache for Salvage tables and elasticity beliefs with TTL"""

    def __init__(self, ttl_hours: int = 24):  # Default 1 day
        self.ttl_hours = ttl_hours
        self.salvage_cache: Dict[str, CacheEntry] = {}
        self.elasticity_cache: Dict[str, CacheEntry] = {}
        self.learner = ElasticityLearner()
        self.generator = SalvageGenerator(SalvageTableConfig())

    def get_salvage(self, asin: str) -> Optional[SalvageEstimate]:
        """Get salvage table; None if not in cache or stale"""
        if asin not in self.salvage_cache:
            return None
        entry = self.salvage_cache[asin]
        if entry.is_stale():
            del self.salvage_cache[asin]
            return None
        return entry.data

    def set_salvage(self, asin: str, estimate: SalvageEstimate):
        """Store salvage estimate in cache"""
        self.salvage_cache[asin] = CacheEntry(
            data=estimate,
            computed_at=datetime.now(),
            ttl_hours=self.ttl_hours,
        )

    def get_elasticity(self, asin: str) -> Optional[ElasticityBelief]:
        """Get elasticity belief from cache"""
        if asin not in self.elasticity_cache:
            return None
        entry = self.elasticity_cache[asin]
        return entry.data  # No staleness check; beliefs don't expire

    def set_elasticity(self, asin: str, belief: ElasticityBelief):
        """Store elasticity belief"""
        self.elasticity_cache[asin] = CacheEntry(
            data=belief,
            computed_at=datetime.now(),
            ttl_hours=999999,  # Beliefs are cumulative, don't expire
        )

    def expedited_salvage_generate(
        self,
        asin: str,
        price: float,
        cost: float,
        demand_mean_weekly: float,
        category: str,
    ) -> Optional[SalvageEstimate]:
        """Fast in-memory salvage table generation"""
        # Try to get elasticity belief; if none, init from category prior
        belief = self.get_elasticity(asin)
        if belief is None:
            belief = self.learner.initialize_belief(asin, category)

        return self.generator.generate_salvage_table(
            asin, price, cost, demand_mean_weekly, belief
        )


class ItemFactory:
    """
    Assembles Item objects from cache and catalog data.
    Handles CIV and salvage staleness fallbacks.
    """

    def __init__(self):
        self.civ_cache = CIVCache(ttl_hours=168)       # 7 days
        self.salvage_cache = SalvageCache(ttl_hours=24)  # 1 day

    def resolve_item_for_eom(
        self,
        asin: str,
        catalog_features: ItemCatalogFeatures,
        item_base: Item,  # Base item with distributions and costs
    ) -> Item:
        """
        Resolve Item for EOM solver with live CIV and salvage.

        Args:
            asin: Item identifier
            catalog_features: Item catalog (velocity, category, price)
            item_base: Base Item with demand_dist, cost, price already set

        Returns:
            Item with lambda_ and salvage_table populated from caches or fallback
        """

        # --- Resolve CIV (lambda_) ---
        civ_est = self.civ_cache.get(asin)
        if civ_est is None or civ_est.computed_at < datetime.now() - timedelta(hours=168):
            # CIV stale/missing: try expedited compute
            civ_est = self.civ_cache.expedited_compute(catalog_features)
            if civ_est:
                self.civ_cache.set(asin, civ_est)
                civ_lambda = civ_est.lambda_value
            else:
                # Fallback to prior
                civ_lambda = 0.87
        else:
            civ_lambda = civ_est.lambda_value

        # --- Resolve Salvage Table ---
        salvage_est = self.salvage_cache.get_salvage(asin)
        if salvage_est is None or salvage_est.computed_at < datetime.now() - timedelta(hours=24):
            # Salvage stale/missing: try expedited generate
            salvage_est = self.salvage_cache.expedited_salvage_generate(
                asin,
                price=catalog_features.price,
                cost=item_base.c,
                demand_mean_weekly=self._extract_demand_mean(item_base),
                category=catalog_features.category,
            )
            if salvage_est:
                self.salvage_cache.set_salvage(asin, salvage_est)
                salvage_table = salvage_est.salvage_table
            else:
                # Fallback to static dummy table
                salvage_table = generate_dummy_salvage_table()
        else:
            salvage_table = salvage_est.salvage_table

        # --- Populate Item with resolved values ---
        item_base.lambda_ = civ_lambda
        item_base.salvage_table = salvage_table

        return item_base

    @staticmethod
    def _extract_demand_mean(item: Item) -> float:
        """Extract mean weekly demand from item's demand distributions"""
        if not item.demand_dist or len(item.demand_dist) == 0:
            return 100.0  # Default if no dist

        total_mean = 0.0
        count = 0
        for dist in item.demand_dist:
            if dist is not None and hasattr(dist, 'mean'):
                total_mean += dist.mean
                count += 1

        if count > 0:
            return total_mean / count
        return 100.0


def demo():
    """Demonstrate item factory with CIV and salvage resolution"""

    print("\n" + "=" * 100)
    print("ITEM FACTORY DEMO")
    print("=" * 100)

    factory = ItemFactory()

    # Create sample items
    sample_asins = ["ASIN-001-MILK", "ASIN-008-SALT"]
    catalog_features_map = {
        "ASIN-001-MILK": ItemCatalogFeatures(
            asin="ASIN-001-MILK",
            category="Dairy",
            subcategory="milk",
            weekly_velocity_units=120.0,
            demand_cv=0.08,
            price=3.50,
        ),
        "ASIN-008-SALT": ItemCatalogFeatures(
            asin="ASIN-008-SALT",
            category="Pantry",
            subcategory="salt",
            weekly_velocity_units=15.0,
            demand_cv=0.25,
            price=2.00,
        ),
    }

    print("\nResolving items with CIV and salvage caches:\n")

    for asin in sample_asins:
        # Generate a base item from EOM module
        item_base = generate_dummy_item(asin)
        catalog = catalog_features_map[asin]

        # Resolve through factory
        item_resolved = factory.resolve_item_for_eom(asin, catalog, item_base)

        print(f"Item: {asin}")
        print(f"  Category: {catalog.category}")
        print(f"  Velocity: {catalog.weekly_velocity_units:.0f} units/week")
        print(f"  CIV (lambda_): ${item_resolved.lambda_:.2f}")
        print(f"  Salvage table: {len(item_resolved.salvage_table)} weeks defined")
        if item_resolved.salvage_table:
            week0 = item_resolved.salvage_table.get(0, {})
            inv1000 = week0.get(1000, 0.0)
            print(f"  Salvage at (week=0, inv=1000): ${inv1000:.2f}")
        print()

    print("=" * 100)
    print("CACHE STATUS")
    print("=" * 100)
    print(f"CIV cache entries: {len(factory.civ_cache.cache)}")
    print(f"Salvage cache entries: {len(factory.salvage_cache.salvage_cache)}")
    print(f"Elasticity beliefs: {len(factory.salvage_cache.elasticity_cache)}")

    return factory


if __name__ == "__main__":
    demo()
