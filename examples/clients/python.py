import os

from openai import OpenAI

client = OpenAI(
    base_url=os.getenv("OPENAI_BASE_URL", "http://127.0.0.1:8080/v1"),
    api_key=os.getenv("OPENAI_API_KEY", "<LOCAL_APP_KEY>"),  # Use the default local key, not the dashboard key.
)

response = client.chat.completions.create(
    model="auto",
    messages=[{"role": "user", "content": "Hello from Spillway"}],
)
print(response.choices[0].message.content)
