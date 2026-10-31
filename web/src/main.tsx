import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { primeFirstReads } from "./api";
import { App } from "./App";
import { applyTheme, readTheme } from "./theme";
import { ErrorBoundary } from "./ui";

import "./fonts.css";
import "./styles.css";

primeFirstReads();
applyTheme(readTheme());

const root = document.querySelector("#root");
if (!root) {
  throw new Error("index.html has no #root element");
}

createRoot(root).render(
  <StrictMode>
    <ErrorBoundary>
      <App />
    </ErrorBoundary>
  </StrictMode>,
);
