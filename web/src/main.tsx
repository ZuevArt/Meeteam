import React from "react";
import ReactDOM from "react-dom/client";
import App from "./App";
import ParserLab from "./ParserLab";
import "./styles.css";

const isLabPage = window.location.pathname.replace(/\/+$/, "") === "/parser-lab";
document.title = isLabPage ? "Meeteam — лаборатория парсера" : "Meeteam — каталог событий";

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    {isLabPage ? <ParserLab /> : <App />}
  </React.StrictMode>,
);
