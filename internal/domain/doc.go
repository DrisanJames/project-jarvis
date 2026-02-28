// Package domain defines the core business types for the IGNITE mailing platform.
//
// Types in this package are pure value objects with no behavior, no database
// dependencies, and no HTTP concerns. They are the shared language between
// handlers, services, and repositories.
//
// Rules for this package:
//   - No imports from other internal/ packages
//   - No *sql.DB, no http.Request, no context.Context in struct fields
//   - JSON/DB tags are allowed (they're metadata, not behavior)
//   - Validation methods are allowed (they're pure functions on the type)
//   - Constants and enums belong here
package domain
