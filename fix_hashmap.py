import re

with open('hashmap.go', 'r') as f:
    content = f.read()

# Fix the bad replacement
bad_replacement = """	h := &HashMap{
		s := h.state.Load()
		if s.next != nil {
			h.helpMigrateAll(s)
		}
		cfg: cfg,
	}"""
good_replacement = """	h := &HashMap{
		cfg: cfg,
	}"""
content = content.replace(bad_replacement, good_replacement)

# Replace the helpMigrate calls
content = content.replace('h.helpMigrate(s, (hashKey(key)>>16)&(s.size-1))', 'h.helpMigrateAll(s)')

with open('hashmap.go', 'w') as f:
    f.write(content)
