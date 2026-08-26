package main

// groupBinding 表示一个分组当前绑定的可用账号集合。
type groupBinding struct {
	group    sub2APIGroup
	accounts map[int64]Channel
}

func buildGroupBindings(channels []Channel) map[int64]*groupBinding {
	bindings := make(map[int64]*groupBinding, len(channels))
	for _, channel := range channels {
		binding := bindings[channel.Group.ID]
		if binding == nil {
			binding = &groupBinding{
				group:    channel.Group,
				accounts: make(map[int64]Channel),
			}
			bindings[channel.Group.ID] = binding
		}
		binding.accounts[channel.AccountID] = channel
	}
	return bindings
}

func (b *groupBinding) onlyAccount() (Channel, bool) {
	if b == nil || len(b.accounts) != 1 {
		return Channel{}, false
	}
	for _, account := range b.accounts {
		return account, true
	}
	return Channel{}, false
}
