# SmartLoad Optimization API

A REST API service that optimizes truck load selection by finding the optimal combination of orders that maximizes carrier revenue while respecting weight, volume, hazmat, route, and time-window constraints.

## How to run

```bash
git clone https://github.com/<username>/teleport.git
cd teleport
docker compose up --build
```

The service will be available at `http://localhost:8080`

## Health check

```bash
curl http://localhost:8080/healthz
```

Response:
```json
{"status":"healthy"}
```

## API Endpoint

### POST /api/v1/load-optimizer/optimize

Returns the optimal combination of orders that maximizes payout while respecting all constraints.

**Request Body:**
```json
{
  "truck": {
    "id": "truck-123",
    "max_weight_lbs": 44000,
    "max_volume_cuft": 3000
  },
  "orders": [
    {
      "id": "ord-001",
      "payout_cents": 250000,
      "weight_lbs": 18000,
      "volume_cuft": 1200,
      "origin": "Los Angeles, CA",
      "destination": "Dallas, TX",
      "pickup_date": "2025-12-05",
      "delivery_date": "2025-12-09",
      "is_hazmat": false
    }
  ]
}
```

**Response:**
```json
{
  "truck_id": "truck-123",
  "selected_order_ids": ["ord-001", "ord-002"],
  "total_payout_cents": 430000,
  "total_weight_lbs": 30000,
  "total_volume_cuft": 2100,
  "utilization_weight_percent": 68.18,
  "utilization_volume_percent": 70.0
}
```

## Example request

```bash
curl -X POST http://localhost:8080/api/v1/load-optimizer/optimize \
  -H "Content-Type: application/json" \
  -d @sample-request.json
```

## Implementation Details

### Algorithm
Dynamic Programming with bitmask and **early pruning**:

- Pre-computes weight, volume, and payout for all 2^n subsets using subset DP
- **Pruning optimization:** During precomputation, subsets exceeding truck capacity are marked invalid immediately, skipping expensive hazmat/route compatibility checks
- Complexity: O(2^n × n) for precomputation, O(2^n) for optimal selection
- Handles up to 22 orders efficiently (4.2M subsets)

### Additional Features
- **Stateless:** No database, in-memory only
- **Caching:** LRU cache with 5-minute TTL for optimization results
- **Money handling:** Integer cents only (no floating point)
- **Hazmat compatibility:** Hazmat loads can only be combined with other hazmat loads
- **Route validation:** All orders in a combination must share the same origin and destination
