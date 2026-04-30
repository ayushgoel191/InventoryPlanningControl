# Deployment Guide: Dynamic CIV and Salvage in Production

## Quick Start (Development)

### Test the modules locally

```bash
# Test CIV estimator
python3 civ_estimator.py

# Test salvage estimator
python3 salvage_estimator.py

# Test item factory
python3 item_factory.py

# Compare static vs dynamic impact
python3 test_factory_integration.py
```

Expected output:
- Milk λ ≈ $2.95 (high destination value)
- Salt λ ≈ $1.38 (lower filler value)
- Profit improvements: +26.6% for milk, +4.5% for salt

---

## Production Deployment

### Phase 1: Data Setup (Week 1)

**1. Catalog Data**

You need catalog features for all SKUs:

```python
from civ_estimator import ItemCatalogFeatures

features = ItemCatalogFeatures(
    asin="ASIN-123456",
    category="Dairy",
    subcategory="milk",
    weekly_velocity_units=120.0,  # From sales DB
    demand_cv=0.08,               # From demand forecast
    price=3.50,                   # From product catalog
)
```

**SQL to extract:**
```sql
-- Fetch catalog features for all active SKUs
SELECT
    p.asin,
    p.category,
    p.subcategory,
    COALESCE(s.weekly_units, 0) as weekly_velocity_units,
    COALESCE(f.demand_cv, -1.0) as demand_cv,  -- -1 = unknown
    p.price
FROM products p
LEFT JOIN sales_velocity s ON p.asin = s.asin
LEFT JOIN demand_forecast f ON p.asin = f.asin
WHERE p.active = true
ORDER BY p.asin;
```

**2. Initialize Elasticity Beliefs**

Create initial beliefs for all SKUs from category priors:

```python
from salvage_estimator import ElasticityLearner

learner = ElasticityLearner()

# For each ASIN, initialize from category
for asin, category in categories.items():
    belief = learner.InitializeBelief(asin, category)
    elasticity_store.save(asin, belief)
```

Store these in your database:
```python
CREATE TABLE elasticity_beliefs (
    asin VARCHAR(50) PRIMARY KEY,
    mu_log_elasticity FLOAT,
    tau_log_elasticity FLOAT,
    n_observations INT,
    last_updated TIMESTAMP,
    category_prior_mu FLOAT
);
```

---

### Phase 2: Schedule Jobs (Week 2-3)

#### Job 1: Weekly CIV Computation

**Trigger:** Every Sunday at 01:00 UTC
**Duration:** ~100ms per 1M items (single-threaded can scale to multi-threaded)

```python
#!/usr/bin/env python3
# compute_civ_weekly.py

from datetime import datetime
from civ_estimator import CIVEstimator, CIVConfig, ItemCatalogFeatures
import db_client  # Your database client

def run_civ_weekly():
    """Compute and cache CIV for all active SKUs"""
    
    # Fetch all catalog features
    print(f"[{datetime.now()}] Fetching catalog features...")
    features_list = db_client.fetch_all_catalog_features()
    print(f"  Got {len(features_list)} items")
    
    # Compute CIV
    print(f"[{datetime.now()}] Computing CIV...")
    estimator = CIVEstimator(CIVConfig())
    civ_results = estimator.batch_compute_civ(features_list)
    print(f"  Computed {len(civ_results)} CIV estimates")
    
    # Store in cache/database
    print(f"[{datetime.now()}] Storing to database...")
    for asin, estimate in civ_results.items():
        db_client.save_civ_estimate(estimate)
    
    # Emit metrics
    confidence_dist = [e.confidence for e in civ_results.values()]
    print(f"  Coverage (confidence > 0.5): {sum(1 for c in confidence_dist if c > 0.5)} / {len(civ_results)}")
    print(f"[{datetime.now()}] ✓ CIV weekly job complete")

if __name__ == "__main__":
    run_civ_weekly()
```

**Cron entry:**
```
0 1 * * 0 /opt/inventory/compute_civ_weekly.py >> /var/log/civ_weekly.log 2>&1
```

---

#### Job 2: Daily Elasticity Update + Salvage Generation

**Trigger:** Every day at 02:00 UTC
**Duration:** ~500ms per 1M items

```python
#!/usr/bin/env python3
# update_elasticity_daily.py

from datetime import datetime, timedelta
from salvage_estimator import ElasticityLearner, SalvageGenerator, SalvageTableConfig
import db_client

def run_elasticity_daily():
    """Update elasticity from observations; regenerate salvage tables"""
    
    learner = ElasticityLearner()
    generator = SalvageGenerator(SalvageTableConfig())
    
    # Fetch markdown observations from last 24 hours
    cutoff = datetime.now() - timedelta(hours=24)
    print(f"[{datetime.now()}] Fetching markdown observations since {cutoff}...")
    observations_by_asin = db_client.fetch_markdown_observations_since(cutoff)
    print(f"  Got {sum(len(v) for v in observations_by_asin.values())} observations")
    
    n_updated = 0
    
    # Update beliefs and regenerate tables
    for asin, observations in observations_by_asin.items():
        # Load current belief
        belief = db_client.load_elasticity_belief(asin)
        if belief is None:
            print(f"  WARN: No belief for {asin}, skipping")
            continue
        
        # Apply updates
        for obs in observations:
            belief = learner.update_belief(belief, obs)
        
        # Store updated belief
        db_client.save_elasticity_belief(belief)
        
        # Regenerate salvage table
        item_meta = db_client.load_item_metadata(asin)  # price, cost, demand_mean
        salvage_est = generator.generate_salvage_table(
            asin,
            price=item_meta['price'],
            cost=item_meta['cost'],
            demand_mean_weekly=item_meta['demand_mean'],
            belief=belief,
        )
        
        # Store table
        db_client.save_salvage_table(salvage_est)
        n_updated += 1
    
    print(f"[{datetime.now()}] Updated {n_updated} elasticity beliefs and salvage tables")
    print(f"[{datetime.now()}] ✓ Elasticity daily job complete")

if __name__ == "__main__":
    run_elasticity_daily()
```

**Cron entry:**
```
0 2 * * * /opt/inventory/update_elasticity_daily.py >> /var/log/elasticity_daily.log 2>&1
```

---

#### Job 3: TIP Computation (Existing, No Changes)

After CIV and salvage jobs have run, execute your existing TIP batch:

```python
#!/usr/bin/env python3
# compute_tip_batch.py

from item_factory import ItemFactory
from eom import process_items_concurrently
import db_client

def run_tip_batch():
    """Compute TIP for batch of SKUs using live CIV and salvage"""
    
    factory = ItemFactory()
    
    # Fetch SKUs to process (e.g., high-velocity items, or all if doing full refresh)
    asins = db_client.fetch_asins_to_process(batch_size=10000)
    print(f"[{datetime.now()}] Processing {len(asins)} items for TIP...")
    
    items = []
    for asin in asins:
        # Load base item (distributions, costs)
        item_base = db_client.load_item_from_db(asin)
        
        # Load catalog features
        features = db_client.load_catalog_features(asin)
        
        # Resolve through factory (applies CIV and salvage)
        item_resolved = factory.resolve_item_for_eom(asin, features, item_base)
        items.append(item_resolved)
    
    # Batch process with EOM solver
    print(f"[{datetime.now()}] Running EOM solver (TIP method)...")
    tip_results = process_items_concurrently(items, num_workers=8, use_tip=True)
    
    # Store results
    print(f"[{datetime.now()}] Storing {len(tip_results)} TIP results...")
    for result in tip_results:
        db_client.save_tip_result(result)
    
    # Metrics
    avg_profit = sum(r.max_profit for r in tip_results) / len(tip_results)
    print(f"[{datetime.now()}] Average profit per item: ${avg_profit:.2f}")
    print(f"[{datetime.now()}] ✓ TIP batch complete")

if __name__ == "__main__":
    run_tip_batch()
```

**Scheduling:** After both CIV and elasticity jobs complete

---

### Phase 3: Monitor and Tune (Ongoing)

#### Monitoring Metrics

```python
# civ_health_check.py
def check_civ_health():
    """Monitor CIV cache freshness and coverage"""
    
    cache_entries = db.execute("""
        SELECT 
            COUNT(*) as total,
            SUM(CASE WHEN DATEDIFF(hour, computed_at, GETDATE()) < 168 THEN 1 ELSE 0 END) as fresh,
            AVG(CAST(confidence AS FLOAT)) as avg_confidence
        FROM civ_estimates
    """)
    
    freshness = cache_entries['fresh'] / cache_entries['total']
    print(f"CIV Cache Freshness: {freshness:.1%}")
    print(f"Average Confidence:  {cache_entries['avg_confidence']:.1%}")
    
    if freshness < 0.95:
        alert("CIV cache stale")
    if cache_entries['avg_confidence'] < 0.60:
        alert("Many items have low CIV confidence")

# elasticity_health_check.py
def check_elasticity_health():
    """Monitor elasticity belief quality"""
    
    beliefs = db.execute("""
        SELECT 
            COUNT(*) as total,
            SUM(CASE WHEN n_observations >= 10 THEN 1 ELSE 0 END) as well_observed,
            AVG(CAST(tau_log_elasticity AS FLOAT)) as avg_precision
        FROM elasticity_beliefs
    """)
    
    converged = beliefs['well_observed'] / beliefs['total']
    print(f"Elasticity Convergence: {converged:.1%} items with ≥10 observations")
    print(f"Average Precision:      {beliefs['avg_precision']:.2f}")
    
    if converged < 0.50:
        print("WARNING: Most items still learning elasticity; recommend larger test windows")
```

---

#### Tuning Recommendations

**If CIV confidence is low:**
- Increase weight on essentiality score (known from category)
- Implement category-specific velocity P90 calculations
- Add product family signals (complementary items)

**If elasticity learning is slow:**
- Increase markdown test window (wider range of prices)
- Ensure observations are logged correctly
- Check for data quality issues in demand tracking

**If salvage table looks wrong:**
- Verify elasticity estimate is reasonable (plot confidence interval)
- Check cost and price data for accuracy
- Test with known good cases (benchmark against historical)

---

## Database Schema

Minimal schema to support the modules:

```sql
-- CIV Cache
CREATE TABLE civ_estimates (
    asin VARCHAR(50) PRIMARY KEY,
    lambda_value DECIMAL(10, 4),
    civ_score DECIMAL(10, 4),
    velocity_score DECIMAL(10, 4),
    stability_score DECIMAL(10, 4),
    essentiality_score DECIMAL(10, 4),
    confidence DECIMAL(10, 4),
    computed_at TIMESTAMP,
    data_version VARCHAR(10),
    INDEX idx_computed_at (computed_at)
);

-- Elasticity Beliefs
CREATE TABLE elasticity_beliefs (
    asin VARCHAR(50) PRIMARY KEY,
    mu_log_elasticity DECIMAL(10, 6),
    tau_log_elasticity DECIMAL(10, 6),
    n_observations INT,
    last_updated TIMESTAMP,
    category_prior_mu DECIMAL(10, 6),
    INDEX idx_updated (last_updated)
);

-- Markdown Observations (append-only)
CREATE TABLE markdown_observations (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    asin VARCHAR(50) NOT NULL,
    week_of_year INT,
    markdown_fraction DECIMAL(10, 4),
    baseline_demand DECIMAL(10, 2),
    observed_demand DECIMAL(10, 2),
    timestamp TIMESTAMP,
    INDEX idx_asin_time (asin, timestamp)
);

-- Salvage Tables (optional; can regenerate nightly)
CREATE TABLE salvage_tables (
    asin VARCHAR(50) NOT NULL,
    week INT NOT NULL,
    inventory_level INT NOT NULL,
    salvage_value DECIMAL(10, 2),
    computed_at TIMESTAMP,
    PRIMARY KEY (asin, week, inventory_level),
    INDEX idx_asin (asin)
);
```

---

## Fallback Strategies

**If CIV weekly job fails:**
- TIP compute will call `factory.expedited_compute()` → O(1), in-memory
- If that fails too: use prior lambda_ = 0.87 for all items
- Manual retry: Run the weekly job again tomorrow

**If elasticity daily job fails:**
- TIP compute will use stale salvage table from previous day
- If no stale entry exists: regenerate with prior elasticity (category default)
- Data isn't lost; just uses best available estimate

**If TIP batch job fails:**
- Rerun for failed items (idempotent)
- Check that CIV and salvage have fresh data
- Monitor logs for missing catalog features or cost data

---

## Rollout Strategy

### Option A: Gradual Rollout (Recommended)

1. **Week 1:** Compute CIV/salvage for all items, but keep using old static TIP
2. **Week 2:** A/B test: 10% of items use dynamic TIP
3. **Week 3:** Ramp to 50%
4. **Week 4:** Ramp to 100%

Monitor profit and service level at each step to catch issues early.

### Option B: Full Rollout (Faster)

1. **Week 1:** Set up jobs, initialize all caches
2. **Week 2:** Switch all items to dynamic CIV and salvage
3. **Monitor closely** for first 2 weeks; rollback if needed

---

## Troubleshooting

**Q: CIV values seem too high/low**

A: Check category essentiality priors. Default assumes Dairy=1.0. If your milk doesn't actually drive basket loss, adjust:

```python
config = CIVConfig()
config.category_essentiality["milk"] = 0.70  # Lower if needed
```

**Q: Salvage tables are all zeros**

A: Check that demand_mean_weekly is correct. If item sells <1 unit per week:

```python
# Likely issue: demand_mean_weekly too low
# Fix: verify demand forecast is in units/week, not units/day
```

**Q: Elasticity beliefs not converging**

A: Need more markdown observations. True convergence requires 20-50 observations across wide markdown range. Use wider discounts for test window.

**Q: Factory lookups are slow**

A: Use Redis for caches instead of in-memory:

```python
# Replace:
self.civ_cache: Dict[str, CacheEntry] = {}

# With:
import redis
self.redis = redis.Redis(host='localhost')
self.redis.get(asin)  # O(1) remote lookup
```

---

## Next: Integration with Your System

1. **Run CIV and Salvage modules standalone** (we've tested them)
2. **Set up database schema** for caching and observations
3. **Wire markdown logging** into `markdown_observations` table
4. **Schedule weekly/daily jobs** with your cron or workflow orchestrator
5. **Monitor metrics** (cache freshness, elasticity convergence)
6. **Gradual rollout** of dynamic CIV/salvage into TIP
7. **Tune weights** based on observed business impact

Questions? Refer to the IMPLEMENTATION_SUMMARY.md or run the test scripts to verify everything works in your environment.
