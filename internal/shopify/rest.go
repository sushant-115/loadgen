package shopify

import (
	"net/http"
	"strconv"
)

// resolveShop picks the ?shop= query value if it matches a configured shop,
// else the first configured shop.
func (s *Store) resolveShop(r *http.Request) string {
	want := r.URL.Query().Get("shop")
	for _, sh := range s.cfg.Shops {
		if sh.Domain == want {
			return want
		}
	}
	return s.cfg.Shops[0].Domain
}

func queryLimit(r *http.Request) int {
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 50
}

// HandleProducts serves GET /admin/api/2024-01/products.json
func HandleProducts(s *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shop := s.resolveShop(r)
		writeJSON(w, http.StatusOK, map[string]any{"products": s.ListProducts(shop, queryLimit(r))})
	}
}

// HandleOrders serves GET /admin/api/2024-01/orders.json (newest first)
func HandleOrders(s *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shop := s.resolveShop(r)
		writeJSON(w, http.StatusOK, map[string]any{"orders": s.ListOrders(shop, queryLimit(r))})
	}
}

// HandleCustomers serves GET /admin/api/2024-01/customers.json
func HandleCustomers(s *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shop := s.resolveShop(r)
		writeJSON(w, http.StatusOK, map[string]any{"customers": s.ListCustomers(shop, queryLimit(r))})
	}
}

// HandleShop serves GET /admin/api/2024-01/shop.json
func HandleShop(s *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		domain := s.resolveShop(r)
		gateway := ""
		for _, sh := range s.cfg.Shops {
			if sh.Domain == domain {
				gateway = sh.Gateway
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"shop": map[string]any{
			"name":            domain,
			"myshopify_domain": domain,
			"primary_gateway": gateway,
			"currency":        "USD",
			"plan_name":       "shopify_plus",
		}})
	}
}
