import json

log_file = "/Users/z3robit/.gemini/antigravity/brain/b8f6e285-ef6f-4c5c-9b9e-2dbd240c0e84/.system_generated/logs/transcript_full.jsonl"
with open(log_file, "r") as f:
    for line in f:
        try:
            entry = json.loads(line)
            if entry.get("source") == "MODEL" and "tool_calls" in entry:
                for tc in entry["tool_calls"]:
                    name = tc["name"]
                    if name in ["write_to_file", "replace_file_content"]:
                        args = tc.get("args", {})
                        target = args.get("TargetFile", "")
                        if "hashmap_ops.go" in target:
                            print(f"[{entry['step_index']}] Modified hashmap_ops.go using {name}")
                        elif "hashmap.go" in target:
                            print(f"[{entry['step_index']}] Modified hashmap.go using {name}")
            elif entry.get("source") == "SYSTEM" and entry.get("type") == "TOOL_RESPONSE":
                content = entry.get("content", "")
                if "File Path:" in content and "hashmap_ops.go" in content:
                    print(f"[{entry['step_index']}] Viewed hashmap_ops.go")
                if "File Path:" in content and "hashmap.go" in content:
                    print(f"[{entry['step_index']}] Viewed hashmap.go")
        except:
            pass
