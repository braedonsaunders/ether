import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import App from "./App";
import { startLocale } from "./i18n";
import "./styles.css";

async function start() {
  // `vite --mode mock` runs the UI against the in-browser fake backend.
  // MODE is a compile-time constant, so production builds drop this branch
  // and never emit the mock chunk.
  if (import.meta.env.MODE === "mock") {
    const { installMockBackend } = await import("./mock/install");
    installMockBackend();
  }
  // Load the owner's language before the first paint so nothing flashes in
  // English; a catalogue that fails to load falls back to English.
  await startLocale();
  createRoot(document.getElementById("root")!).render(
    <StrictMode>
      <BrowserRouter basename="/ether">
        <App />
      </BrowserRouter>
    </StrictMode>,
  );
}

void start();
