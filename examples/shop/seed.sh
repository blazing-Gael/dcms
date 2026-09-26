#!/usr/bin/env bash
# Seed the shop demo with a catalog + a customer, then print ready-to-paste curls
# for the live part of the demo. Authenticates as the admin via the panel login (so
# no API token is needed). Usage: seed.sh [admin-email] [admin-password]
set -u
BASE=http://localhost:8080
API=$BASE/api/v1
EMAIL="${1:-you@shop.test}"
PASS="${2:-shoppass}"
J=$(mktemp)
id_of() { grep -oE '"id":"[^"]+"' | head -1 | sed 's/.*:"//; s/"$//'; }

# Log in through the admin panel to get a session cookie the API also accepts.
curl -s -c "$J" "$BASE/__admin/login" >/dev/null
CSRF=$(grep dcms_admin_csrf "$J" | awk '{print $NF}')
curl -s -b "$J" -c "$J" -X POST "$BASE/__admin/login" \
  --data-urlencode "email=$EMAIL" --data-urlencode "password=$PASS" --data-urlencode "csrf=$CSRF" -o /dev/null
AUTH=(-b "$J" -H "Content-Type: application/json")
post() { curl -s "${AUTH[@]}" -X POST "$API/$1" -d "$2"; }

echo "categories..."
C_COF=$(post categories '{"name":"Coffee","slug":"coffee"}' | id_of)
C_TEA=$(post categories '{"name":"Tea","slug":"tea"}' | id_of)

echo "product image..."
printf '%s' 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==' | base64 -d > /tmp/prod.png
IMG=$(curl -s -b "$J" -F "file=@/tmp/prod.png" -F "alt=Product" "$BASE/__media" | id_of)

echo "products..."
P1=$(post products "{\"name\":\"House Blend 250g\",\"slug\":\"house-blend\",\"price\":\"12.50\",\"cost\":\"6.20\",\"stock\":20,\"image\":\"$IMG\",\"category\":\"$C_COF\"}" | id_of)
P2=$(post products "{\"name\":\"Single Origin 250g\",\"slug\":\"single-origin\",\"price\":\"18.00\",\"cost\":\"9.40\",\"stock\":5,\"image\":\"$IMG\",\"category\":\"$C_COF\"}" | id_of)
P3=$(post products "{\"name\":\"Green Tea 100g\",\"slug\":\"green-tea\",\"price\":\"9.00\",\"cost\":\"4.10\",\"stock\":12,\"image\":\"$IMG\",\"category\":\"$C_TEA\"}" | id_of)
for p in "$P1" "$P2" "$P3"; do curl -s "${AUTH[@]}" -X POST "$API/products/$p/publish" >/dev/null; done

echo "customer..."
CUST=$(post customers '{"name":"Ada Lovelace","email":"ada@example.com"}' | id_of)

echo "staff user (for the role-scoped transition demo)..."
# Re-fetch a CSRF token for the panel user-create form.
SCSRF=$(grep dcms_admin_csrf "$J" | awk '{print $NF}')
curl -s -b "$J" -X POST "$BASE/__admin/users" \
  --data-urlencode "email=staff@shop.test" --data-urlencode "password=staffpass" \
  --data-urlencode "name=Sam Staff" --data-urlencode "roles=staff" --data-urlencode "csrf=$SCSRF" -o /dev/null
rm -f "$J"

cat <<EOF

Seeded. IDs for the demo:
  customer       $CUST
  house-blend    $P1   (stock 20, \$12.50)
  single-origin  $P2   (stock 5,  \$18.00)   <- low stock: use this for the oversell shot
  green-tea      $P3   (stock 12, \$9.00)

── Place an order (server computes the total, decrements stock, "charges") ──
curl -s -H 'Content-Type: application/json' -X POST $API/orders -d '{
  "customer":"$CUST","pay_token":"tok_live_ok",
  "items":[{"product":"$P1","qty":2},{"product":"$P3","qty":1}]
}'

── Oversell is refused, and NOTHING is written (order + stock roll back together) ──
curl -s -i -H 'Content-Type: application/json' -X POST $API/orders -d '{
  "customer":"$CUST","pay_token":"tok_live_ok","items":[{"product":"$P2","qty":99}]
}'

── Payment decline is refused the same way ──
curl -s -i -H 'Content-Type: application/json' -X POST $API/orders -d '{
  "customer":"$CUST","pay_token":"declined","items":[{"product":"$P1","qty":1}]
}'

── Idempotency: same key twice = one order, one stock decrement ──
curl -s -H 'Content-Type: application/json' -H 'Idempotency-Key: demo-42' -X POST $API/orders -d '{
  "customer":"$CUST","pay_token":"tok_live_ok","items":[{"product":"$P3","qty":1}]
}'   # run it twice — the second returns the first order, stock drops only once
EOF
