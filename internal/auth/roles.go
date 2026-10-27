// Package auth implements login identities, roles and permissions, password
// hashing, sessions and CSRF tokens.
//
// Authentication is deliberately separate from the member record: a user is
// a login identity keyed by email which may link to a member row. Admins
// sign in with a password today; the users.password_hash column is nullable
// and the login_tokens table exists so that email magic links (and later
// TOTP) can be added for members without schema changes. See
// LoginMethodMagicLink and Store.CreateLoginToken for the extension point.
package auth

import (
	"slices"

	"github.com/clidive410/membership/internal/members"
)

// Role is a named set of permissions, stored in user_roles.
type Role string

// Roles (seeded in the roles table).
const (
	RoleAdmin             Role = "admin"              // committee
	RoleMembershipManager Role = "membership_manager" // type/expiry/BSAC/payments
	RoleMember            Role = "member"             // future self-service
)

// AllRoles lists the known roles.
var AllRoles = []Role{RoleAdmin, RoleMembershipManager, RoleMember}

// ValidRole reports whether r is a known role.
func ValidRole(r Role) bool { return slices.Contains(AllRoles, r) }

// Permission is a single capability checked by route middleware.
type Permission string

// Permissions.
const (
	PermViewMembers     Permission = "members.view"
	PermCreateMembers   Permission = "members.create"
	PermArchiveMembers  Permission = "members.archive"
	PermEditPersonal    Permission = "members.edit.personal"
	PermEditClub        Permission = "members.edit.club"
	PermEditMembership  Permission = "members.edit.membership"
	PermRecordPayments  Permission = "payments.record"
	PermExport          Permission = "members.export"
	PermEmailLists      Permission = "members.emaillists"
	PermViewAudit       Permission = "audit.view"
	PermEditOwnPersonal Permission = "self.edit.personal" // future member self-service
)

var rolePermissions = map[Role][]Permission{
	RoleAdmin: {PermViewMembers, PermCreateMembers, PermArchiveMembers, PermEditPersonal, PermEditClub,
		PermEditMembership, PermRecordPayments, PermExport, PermEmailLists, PermViewAudit, PermEditOwnPersonal},
	RoleMembershipManager: {PermViewMembers, PermEditMembership, PermRecordPayments, PermViewAudit},
	RoleMember:            {PermEditOwnPersonal},
}

// Principal is the authenticated user of a request.
type Principal struct {
	UserID   int64
	Email    string
	MemberID int64 // 0 when the user has no member row (e.g. some admins)
	Roles    []Role
}

// Can reports whether any of the principal's roles grants p.
func (p *Principal) Can(perm Permission) bool {
	if p == nil {
		return false
	}
	for _, r := range p.Roles {
		if slices.Contains(rolePermissions[r], perm) {
			return true
		}
	}
	return false
}

// HasRole reports whether the principal has role r.
func (p *Principal) HasRole(r Role) bool { return p != nil && slices.Contains(p.Roles, r) }

// Actor returns the audit-log actor for this principal.
func (p *Principal) Actor() members.Actor {
	return members.Actor{UserID: p.UserID, Label: p.Email}
}

// EditableGroups returns the member field groups the principal may edit on
// any member record through the admin UI.
func (p *Principal) EditableGroups() []members.FieldGroup {
	var g []members.FieldGroup
	if p.Can(PermEditPersonal) {
		g = append(g, members.GroupPersonal)
	}
	if p.Can(PermEditClub) {
		g = append(g, members.GroupClub)
	}
	if p.Can(PermEditMembership) {
		g = append(g, members.GroupMembership)
	}
	return g
}

// SelfEditableGroups returns the groups a member may edit on their own
// record (future self-service): never type/expiry/BSAC fields.
func (p *Principal) SelfEditableGroups() []members.FieldGroup {
	if p.Can(PermEditOwnPersonal) {
		return []members.FieldGroup{members.GroupPersonal}
	}
	return nil
}
