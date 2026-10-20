import OpenAI from "openai";

const client = new OpenAI({
  baseURL: process.env.OPENAI_BASE_URL ?? "http://127.0.0.1:8080/v1",
  apiKey: process.env.OPENAI_API_KEY ?? "<LOCAL_APP_KEY>", // Use the default local key, not the dashboard key.
});

const response = await client.chat.completions.create({
  model: "auto",
  messages: [{ role: "user", content: "Hello from Spillway" }],
});
console.log(response.choices[0].message.content);
