package main

import (
	"eventpos/internal/caja"
	"eventpos/internal/catalog"
	"eventpos/internal/clientapi"
	"eventpos/internal/customers"
	"eventpos/internal/events"
	"eventpos/internal/kitchen"
	"eventpos/internal/oversight"
	"eventpos/internal/reports"
	"eventpos/internal/sales"
	"eventpos/internal/server"
	"eventpos/internal/users"
)

// modules returns the domain modules mounted into the server, in dependency
// order. This is the single integration point: each domain package exposes a
// constructor returning a server.Module, and they are wired here.
func modules(deps server.Deps) []server.Module {
	return []server.Module{
		users.New(deps),     // L1 operators
		events.New(deps),    // L1 event lifecycle
		catalog.New(deps),   // L2 categories/products/inventory
		customers.New(deps), // L2 attendees/saldo
		caja.New(deps),      // L3 cash sessions
		sales.New(deps),     // L4 atomic sale + split
		kitchen.New(deps),   // L5 pendientes (entregas)
		reports.New(deps),   // L6 dashboard/reports/exports
		oversight.New(deps), // L7 logs/audit/integrity
		clientapi.New(deps), // cliente: auto-pedido por QR (web negra)
	}
}
