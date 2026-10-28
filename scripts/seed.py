#!/usr/bin/env python3
"""Seed a running GenesisDB with realistic e-commerce events for load and live testing.

    GENESISDB_AUTH_TOKEN=... python3 scripts/seed.py [count] [startCustomerId]

Defaults: 250000 events, customer IDs from 0. GDB_URL overrides the base URL
(default http://127.0.0.1:8080). Standard library only, no dependencies.

Generates customers with personal data as reference payloads (erasable), and
orders with items, confirmation, payment, shipment, delivery, returns, failed
payments and cancellations under /order/<uuid>/... Run it again with a higher
start ID to add more without subject collisions.
"""
import json, os, random, sys, time, uuid, urllib.request, concurrent.futures

BASE = os.environ.get("GDB_URL", "http://127.0.0.1:8080")
TOKEN = os.environ["GENESISDB_AUTH_TOKEN"]
TOTAL = int(sys.argv[1]) if len(sys.argv) > 1 else 250_000
START_CUSTOMER = int(sys.argv[2]) if len(sys.argv) > 2 else 0
BATCH = 500
WORKERS = 4

random.seed(42)
FIRST = ["Bruce", "Selina", "Alfred", "Diana", "Clark", "Lois", "Barry", "Hal", "Arthur", "Victor", "Oliver", "Dinah", "Kara", "Zatanna", "John"]
LAST = ["Wayne", "Kyle", "Pennyworth", "Prince", "Kent", "Lane", "Allen", "Jordan", "Curry", "Stone", "Queen", "Lance", "Danvers", "Zatara", "Constantine"]
CITIES = ["Gotham", "Metropolis", "Central City", "Star City", "Coast City", "Bludhaven", "Keystone", "National City", "Smallville", "Themyscira"]
COUNTRIES = ["US", "DE", "AT", "CH", "GB", "FR", "NL", "SE"]
PRODUCTS = [("Tumbler", 2990000.50, "vehicles"), ("Batarang", 149.99, "gear"), ("Grapple gun", 899.00, "gear"), ("Cowl", 1200.00, "apparel"),
            ("Utility belt", 650.00, "apparel"), ("Lasso", 999.99, "gear"), ("Trident", 4500.00, "weapons"), ("Power ring", 99999.00, "artifacts"),
            ("Cape", 320.00, "apparel"), ("Kryptonite shard", 15000.00, "artifacts"), ("Boomerang", 59.99, "gear"), ("Speed suit", 2100.00, "apparel")]
CARRIERS = ["DHL", "UPS", "FedEx", "DPD"]
PAYMENT = ["card", "paypal", "invoice", "sepa"]

def commit(events):
    body = json.dumps({"events": events}).encode()
    req = urllib.request.Request(BASE + "/api/v1/commit", data=body, method="POST",
                                 headers={"Authorization": "Bearer " + TOKEN, "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=120) as r:
        if r.status != 200:
            raise RuntimeError(r.status)
        r.read()

def ev(subject, type_, data, ref=False):
    e = {"source": "io.genesisdb.shop", "subject": subject, "type": "io.genesisdb.shop." + type_, "data": data}
    if ref:
        e["options"] = {"storeDataAsReference": True}
    return e

def customer_lifecycle(rng, cid):
    """A customer with personal data as reference payload, plus a few orders. Returns events."""
    out = []
    first, last = rng.choice(FIRST), rng.choice(LAST)
    cs = f"/customer/{cid}"
    out.append(ev(cs, "customer-registered", {"firstName": first, "lastName": last, "email": f"{first.lower()}.{last.lower()}{rng.randint(1, 9999)}@example.com",
                                              "birthYear": rng.randint(1950, 2005), "newsletter": rng.random() < 0.4}, ref=True))
    out.append(ev(f"{cs}/address", "address-added", {"street": f"{rng.randint(1, 999)} {rng.choice(['Main', 'Park', 'Crime', 'Harbor', 'Elm'])} St",
                                                     "city": rng.choice(CITIES), "zip": f"{rng.randint(10000, 99999)}", "country": rng.choice(COUNTRIES)}, ref=True))
    if rng.random() < 0.3:
        out.append(ev(f"{cs}/address", "address-changed", {"city": rng.choice(CITIES), "country": rng.choice(COUNTRIES)}, ref=True))
    if rng.random() < 0.15:
        out.append(ev(cs, "customer-renamed", {"lastName": rng.choice(LAST)}, ref=True))
    for _ in range(rng.choice([0, 1, 1, 2, 2, 3, 5])):
        oid = str(uuid.UUID(int=rng.getrandbits(128), version=4))
        os_ = f"/order/{oid}"
        items = rng.sample(PRODUCTS, rng.randint(1, 4))
        total = 0.0
        out.append(ev(os_, "order-placed", {"customer": cs, "currency": "EUR", "channel": rng.choice(["web", "app", "pos"])}))
        for pos, (name, price, cat) in enumerate(items, 1):
            qty = rng.randint(1, 3)
            total += price * qty
            out.append(ev(f"{os_}/item/{pos}", "item-added", {"product": name, "category": cat, "quantity": qty, "unitPrice": price, "lineTotal": round(price * qty, 2)}))
        out.append(ev(os_, "order-confirmed", {"total": round(total, 2), "items": len(items)}))
        r = rng.random()
        if r < 0.85:
            out.append(ev(f"{os_}/payment", "payment-received", {"method": rng.choice(PAYMENT), "amount": round(total, 2), "currency": "EUR"}))
            if rng.random() < 0.9:
                out.append(ev(f"{os_}/shipment", "shipment-dispatched", {"carrier": rng.choice(CARRIERS), "tracking": uuid.uuid4().hex[:16].upper(), "parcels": rng.randint(1, 2)}))
                if rng.random() < 0.8:
                    out.append(ev(f"{os_}/shipment", "shipment-delivered", {"signedBy": rng.choice(FIRST)}))
                if rng.random() < 0.05:
                    out.append(ev(f"{os_}/return", "return-requested", {"reason": rng.choice(["damaged", "wrong size", "changed mind", "late"])}))
        elif r < 0.95:
            out.append(ev(f"{os_}/payment", "payment-failed", {"method": rng.choice(PAYMENT), "reason": rng.choice(["declined", "timeout", "fraud-check"])}))
        else:
            out.append(ev(os_, "order-cancelled", {"by": rng.choice(["customer", "shop"])}))
    return out

def generate(total):
    rng = random.Random(42 + START_CUSTOMER)
    cid = START_CUSTOMER
    buf = []
    produced = 0
    while produced < total:
        cid += 1
        for e in customer_lifecycle(rng, cid):
            if produced >= total:
                break
            buf.append(e)
            produced += 1
            if len(buf) == BATCH:
                yield buf
                buf = []
    if buf:
        yield buf

def main():
    started = time.time()
    done = 0
    batches = 0
    with concurrent.futures.ThreadPoolExecutor(max_workers=WORKERS) as pool:
        inflight = set()
        for batch in generate(TOTAL):
            inflight.add(pool.submit(commit, batch))
            if len(inflight) >= WORKERS:
                finished, inflight = concurrent.futures.wait(inflight, return_when=concurrent.futures.FIRST_COMPLETED)
                for f in finished:
                    f.result()
                    done += BATCH
                    batches += 1
            if batches and batches % 20 == 0:
                rate = done / (time.time() - started)
                print(f"\r{done:>8,} / {TOTAL:,} events  {rate:,.0f} ev/s  {time.time() - started:,.0f}s", end="", flush=True)
                batches += 1
        for f in concurrent.futures.as_completed(inflight):
            f.result()
    elapsed = time.time() - started
    print(f"\r{TOTAL:>8,} events in {elapsed:,.1f}s  ({TOTAL / elapsed:,.0f} ev/s)" + " " * 20)

if __name__ == "__main__":
    main()
