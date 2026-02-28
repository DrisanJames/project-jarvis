// Package campaign implements campaign lifecycle management.
//
// The service layer contains all business logic for creating, scheduling,
// sending, and completing email campaigns. It depends on repository interfaces
// defined in this package and should never import from handler/.
//
// Repository implementations live in repository/postgres/ and repository/memory/.
package campaign
