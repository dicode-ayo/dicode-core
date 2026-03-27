name = params.get("name")

log.info(f"Hello, {name}!")
log.info("Storing greeting in kv...")

kv.set("last_greeting", name)
previous = kv.get("previous_name")
if previous:
    log.info(f"Previous greeting was for: {previous}")

kv.set("previous_name", name)

result = {"greeting": f"Hello, {name}!", "name": name}
