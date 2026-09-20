import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { CollectorApp } from "./CollectorApp";
import "./styles.css";

const root = document.getElementById("root");
if (!root) throw new Error("Collector root element is missing");

createRoot(root).render(
  <StrictMode>
    <CollectorApp />
  </StrictMode>,
);

