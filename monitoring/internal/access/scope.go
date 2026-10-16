package access

// Scope is an immutable, request-local grant. Its zero value grants no access.
// Only Authorizer creates authenticated grants; Public permits public groups.
type Scope struct {
	admin    bool
	public   bool
	groupIDs []int64
}

func Public() Scope { return Scope{public: true} }

func Administrator() Scope { return Scope{admin: true} }

func Groups(ids []int64) Scope {
	return Scope{groupIDs: append([]int64(nil), ids...)}
}

func (s Scope) IsAdmin() bool { return s.admin }

func (s Scope) IsPublic() bool { return s.public }

func (s Scope) GroupIDs() []int64 { return append([]int64(nil), s.groupIDs...) }
