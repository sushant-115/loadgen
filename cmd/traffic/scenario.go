package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sync"
)

type scenario struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Actions     []actionSpec `json:"actions"`
}

type actionSpec struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
}

type actionPicker struct {
	actions     []actionSpec
	totalWeight float64
}

func newActionPicker(s scenario) (*actionPicker, error) {
	if len(s.Actions) == 0 {
		return nil, errors.New("scenario has no actions")
	}

	picker := &actionPicker{actions: make([]actionSpec, 0, len(s.Actions))}
	for _, action := range s.Actions {
		if action.Name == "" {
			return nil, errors.New("action name cannot be empty")
		}
		if action.Weight <= 0 {
			continue
		}
		picker.actions = append(picker.actions, action)
		picker.totalWeight += action.Weight
	}

	if len(picker.actions) == 0 || picker.totalWeight <= 0 {
		return nil, errors.New("scenario has no positive action weights")
	}

	return picker, nil
}

func (p *actionPicker) pick() string {
	roll := rand.Float64() * p.totalWeight
	var cumulative float64
	for _, action := range p.actions {
		cumulative += action.Weight
		if roll <= cumulative {
			return action.Name
		}
	}
	return p.actions[len(p.actions)-1].Name
}

func loadScenario(path string) (scenario, error) {
	if path == "" {
		return defaultScenario(), nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return scenario{}, fmt.Errorf("read scenario file: %w", err)
	}

	var cfg scenario
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return scenario{}, fmt.Errorf("parse scenario file: %w", err)
	}

	if cfg.Name == "" {
		cfg.Name = "custom-scenario"
	}

	return cfg, nil
}

func defaultScenario() scenario {
	return scenario{
		Name:        "default-production-like",
		Description: "Weighted API traffic with dependency-aware actions",
		Actions: []actionSpec{
			{Name: "users_list", Weight: 0.28},
			{Name: "users_get", Weight: 0.10},
			{Name: "auth_login", Weight: 0.15},
			{Name: "auth_verify", Weight: 0.05},
			{Name: "orders_create", Weight: 0.20},
			{Name: "orders_list", Weight: 0.10},
			{Name: "orders_get", Weight: 0.05},
			{Name: "users_create", Weight: 0.04},
			{Name: "user_upgrade", Weight: 0.03}, // conversion event
		},
	}
}

type actorState struct {
	mu      sync.Mutex
	users   []string
	orders  []string
	tokens  []string
	maxSize int
}

func newActorState(maxSize int) *actorState {
	return &actorState{maxSize: maxSize}
}

func (s *actorState) addUser(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users = appendTrimmed(s.users, userID, s.maxSize)
}

func (s *actorState) addOrder(orderID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orders = appendTrimmed(s.orders, orderID, s.maxSize)
}

func (s *actorState) addToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = appendTrimmed(s.tokens, token, s.maxSize)
}

func (s *actorState) randomUser() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.users) == 0 {
		return ""
	}
	return s.users[rand.Intn(len(s.users))]
}

func (s *actorState) randomOrder() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.orders) == 0 {
		return ""
	}
	return s.orders[rand.Intn(len(s.orders))]
}

func (s *actorState) randomToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tokens) == 0 {
		return ""
	}
	return s.tokens[rand.Intn(len(s.tokens))]
}

func appendTrimmed(values []string, next string, maxSize int) []string {
	if next == "" {
		return values
	}
	values = append(values, next)
	if len(values) <= maxSize {
		return values
	}
	return values[len(values)-maxSize:]
}

type requestPlan struct {
	Action string
	Method string
	Path   string
	Body   []byte
}

func buildPlan(action string, actors *actorState) requestPlan {
	switch action {
	case "users_list":
		return requestPlan{Action: action, Method: "GET", Path: "/api/users"}
	case "users_get":
		userID := actors.randomUser()
		if userID == "" {
			userID = fmt.Sprintf("%d", rand.Intn(3)+1)
		}
		return requestPlan{Action: action, Method: "GET", Path: "/api/users/" + userID}
	case "users_create":
		body, _ := json.Marshal(map[string]string{
			"name":  fmt.Sprintf("User %s", randomID(6)),
			"email": fmt.Sprintf("%s@example.com", randomID(8)),
		})
		return requestPlan{Action: action, Method: "POST", Path: "/api/users", Body: body}
	case "auth_login":
		usernames := []string{"alice", "bob", "charlie", "demo"}
		body, _ := json.Marshal(map[string]string{
			"username": usernames[rand.Intn(len(usernames))],
			"password": "testpassword123",
		})
		return requestPlan{Action: action, Method: "POST", Path: "/api/auth/login", Body: body}
	case "auth_verify":
		token := actors.randomToken()
		if token == "" {
			token = "invalid-token"
		}
		body, _ := json.Marshal(map[string]string{"token": token})
		return requestPlan{Action: action, Method: "POST", Path: "/api/auth/verify", Body: body}
	case "orders_list":
		return requestPlan{Action: action, Method: "GET", Path: "/api/orders"}
	case "orders_get":
		orderID := actors.randomOrder()
		if orderID == "" {
			orderID = "ORD-001"
		}
		return requestPlan{Action: action, Method: "GET", Path: "/api/orders/" + orderID}
	case "user_upgrade":
		// Conversion event — pick an existing user and upgrade them.
		userID := actors.randomUser()
		if userID == "" {
			userID = fmt.Sprintf("%d", rand.Intn(3)+1)
		}
		return requestPlan{Action: action, Method: "POST", Path: "/api/users/" + userID + "/upgrade"}
	case "orders_create":
		userID := actors.randomUser()
		if userID == "" {
			userID = fmt.Sprintf("%d", rand.Intn(3)+1)
		}
		items := rand.Intn(3) + 1
		orderItems := make([]map[string]any, items)
		for i := range orderItems {
			orderItems[i] = map[string]any{
				"product_id": fmt.Sprintf("P%d", 100+rand.Intn(900)),
				"name":       fmt.Sprintf("Item-%s", randomID(3)),
				"quantity":   rand.Intn(3) + 1,
				"price":      float64(rand.Intn(9000)+500) / 100.0,
			}
		}
		body, _ := json.Marshal(map[string]any{"user_id": userID, "items": orderItems})
		return requestPlan{Action: action, Method: "POST", Path: "/api/orders", Body: body}
	default:
		return requestPlan{}
	}
}
