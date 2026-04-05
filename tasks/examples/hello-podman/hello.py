import platform
import requests

print("Hello from Podman!")
print(f"Python: {platform.python_version()}")
print(f"OS: {platform.system()} {platform.release()}")

r = requests.get("https://httpbin.org/get", timeout=5)
print(f"HTTP check: {r.status_code} {r.json()['origin']}")
