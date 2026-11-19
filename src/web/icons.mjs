const paths = {
  overview: "M3 3h8v8H3z M15 3h6v4h-6z M15 11h6v10h-6z M3 15h8v6H3z",
  keys: "M14 4a5 5 0 1 1-2 9l-7 7H2v-3l7-7a5 5 0 0 1 5-6Z M16 7l1 1",
  sessions: "M3 4h18v12H9l-5 4v-4H3z M7 8h10 M7 12h6",
  stats: "M3 3v18h18 M7 16v-4 M12 16V7 M17 16v-7",
  logs: "M5 3h14v18H5z M8 7h8 M8 11h8 M8 15h5",
  errors: "M12 3 2 20h20z M12 9v5 M12 17h.01",
  models: "M12 2 3 7l9 5 9-5z M3 12l9 5 9-5 M3 17l9 5 9-5",
  cli: "M3 4h18v16H3z M6 8l3 3-3 3 M12 15h5",
  play: "M7 4v16l13-8z",
  site: "M3 4h18v16H3z M3 9h18 M7 6.5h.01 M10 6.5h.01 M7 14h5",
  forward: "m9 5 7 7-7 7",
};
export function icon(name) {
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  for (const [k, v] of Object.entries({
    width: "17",
    height: "17",
    viewBox: "0 0 24 24",
    fill: "none",
    stroke: "currentColor",
    "stroke-width": "1.6",
    "stroke-linecap": "round",
    "stroke-linejoin": "round",
    "aria-hidden": "true",
  }))
    svg.setAttribute(k, v);
  const path = document.createElementNS(ns, "path");
  path.setAttribute("d", paths[name]);
  svg.append(path);
  return svg;
}
