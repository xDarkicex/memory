import json

log_file = "/Users/z3robit/.gemini/antigravity/brain/b8f6e285-ef6f-4c5c-9b9e-2dbd240c0e84/.system_generated/logs/transcript_full.jsonl"

def find_file_content():
    seen_hashmap = False
    seen_ops = False
    
    with open(log_file, "r") as f:
        for line in f:
            try:
                entry = json.loads(line)
                
                # Check tool calls
                if "tool_calls" in entry:
                    for tc in entry["tool_calls"]:
                        if tc["name"] in ["replace_file_content", "multi_replace_file_content", "write_to_file"]:
                            if "hashmap.go" in tc.get("args", {}).get("TargetFile", ""):
                                print(f"Found modification to hashmap.go at step {entry.get('step_index')}")
                            if "hashmap_ops.go" in tc.get("args", {}).get("TargetFile", ""):
                                print(f"Found modification to hashmap_ops.go at step {entry.get('step_index')}")
                
                # Check view_file output
                if entry.get("source") == "SYSTEM" and entry.get("type") == "VIEW_FILE":
                    content = entry.get("content", "")
                    if "File Path:" in content and "hashmap.go" in content and not seen_hashmap:
                        print(f"Found first view of hashmap.go at step {entry.get('step_index')}")
                        seen_hashmap = True
                        with open("original_hashmap.go.txt", "w") as out:
                            out.write(content)
                    if "File Path:" in content and "hashmap_ops.go" in content and not seen_ops:
                        print(f"Found first view of hashmap_ops.go at step {entry.get('step_index')}")
                        seen_ops = True
                        with open("original_hashmap_ops.go.txt", "w") as out:
                            out.write(content)
            except Exception as e:
                pass

find_file_content()
