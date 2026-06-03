const fmt = new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 4 });
const money = new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 2 });

async function api(path) {
  const response = await fetch(path);
  const body = await response.json();
  if (!response.ok) {
    throw new Error(body.error || response.statusText);
  }
  return body;
}

function byId(id) {
  return document.getElementById(id);
}

function setStatus(id, text) {
  byId(id).textContent = text || "";
}

function esc(value) {
  return String(value ?? "").replace(/[&<>"]/g, (char) => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    '"': "&quot;",
  }[char]));
}

function renderMetric(title, value) {
  return [
    '<div class="col-6 col-md-4 col-xl-2">',
    '  <div class="card metric-card border-0 bg-body-tertiary">',
    '    <div class="card-body">',
    `      <div class="small text-body-secondary mb-1">${esc(title)}</div>`,
    `      <div class="fw-semibold">${esc(value)}</div>`,
    "    </div>",
    "  </div>",
    "</div>",
  ].join("");
}

async function loadContracts() {
  const q = byId("contractQuery").value.trim();
  const kind = byId("kindFilter").value;
  const limit = byId("contractLimit").value || "200";
  setStatus("contractStatus", "加载中...");

  const data = await api(`/api/contracts?q=${encodeURIComponent(q)}&kind=${encodeURIComponent(kind)}&limit=${encodeURIComponent(limit)}`);
  const tbody = byId("contractsBody");
  tbody.innerHTML = data.contracts.map((contract) => [
    `<tr class="contract-row" data-code="${esc(contract.code)}">`,
    `  <td>${esc(contract.code)}</td>`,
    `  <td>${esc(contract.kind)}</td>`,
    `  <td>${esc(contract.first_date)}</td>`,
    `  <td>${esc(contract.last_date)}</td>`,
    `  <td class="text-end">${esc(contract.rows)}</td>`,
    "</tr>",
  ].join("")).join("");

  tbody.querySelectorAll("tr").forEach((row) => {
    row.addEventListener("click", () => loadHistory(row.dataset.code));
  });

  setStatus("contractStatus", `${data.count} 个合约`);
  if (data.contracts.length > 0) {
    loadHistory(data.contracts[0].code).catch((error) => setStatus("historyStatus", error.message));
  } else {
    byId("historyBody").innerHTML = "";
    byId("historyTitle").textContent = "历史";
    setStatus("historyStatus", "无结果");
  }
}

async function loadHistory(code) {
  const limit = byId("historyLimit").value || "500";
  setStatus("historyStatus", "加载中...");

  const data = await api(`/api/history?code=${encodeURIComponent(code)}&limit=${encodeURIComponent(limit)}`);
  byId("historyTitle").textContent = `${data.contract.code} 历史`;
  byId("historyBody").innerHTML = data.records.map((record) => [
    "<tr>",
    `  <td>${esc(record.date)}</td>`,
    `  <td>${esc(record.code)}</td>`,
    `  <td class="text-end">${esc(record.open)}</td>`,
    `  <td class="text-end">${esc(record.high)}</td>`,
    `  <td class="text-end">${esc(record.low)}</td>`,
    `  <td class="text-end">${esc(record.close)}</td>`,
    `  <td class="text-end">${esc(record.settle)}</td>`,
    `  <td class="text-end">${esc(record.volume)}</td>`,
    `  <td class="text-end">${esc(record.open_interest)}</td>`,
    `  <td class="text-end">${esc(record.delta)}</td>`,
    "</tr>",
  ].join("")).join("");
  setStatus("historyStatus", `${data.count} 行，区间 ${data.contract.first_date} 到 ${data.contract.last_date}`);
}

async function runBacktest() {
  const params = new URLSearchParams({
    basis_yield: byId("basisYield").value || "0.06",
    roll_days: byId("rollDays").value || "5",
    multiplier: byId("multiplier").value || "200",
    start: byId("startDate").value,
    end: byId("endDate").value,
  });
  setStatus("backtestStatus", "运行中...");

  const data = await api(`/api/backtest?${params.toString()}`);
  byId("backtestSummary").innerHTML = [
    ["总收益", money.format(data.total_profit)],
    ["持有天数", data.holding_days],
    ["移仓次数", data.rolls],
    ["最终合约", data.final_contract],
    ["最终结算", fmt.format(data.final_settle)],
  ].map(([title, value]) => renderMetric(title, value)).join("");

  byId("eventsBody").innerHTML = data.events.map((event) => [
    "<tr>",
    `  <td>${esc(event.date)}</td>`,
    `  <td>${esc(event.action)}</td>`,
    `  <td>${esc(event.code)}</td>`,
    `  <td class="text-end">${fmt.format(event.price)}</td>`,
    `  <td class="text-end">${fmt.format(event.spot_close)}</td>`,
    `  <td class="text-end">${fmt.format(event.annualized_basis * 100)}%</td>`,
    `  <td class="text-end">${event.days_to_expiry}</td>`,
    `  <td class="text-end">${money.format(event.cumulative_profit)}</td>`,
    "</tr>",
  ].join("")).join("");

  setStatus("backtestStatus", `${data.start_date} 到 ${data.end_date}`);
}

byId("refreshContracts").addEventListener("click", () => {
  loadContracts().catch((error) => setStatus("contractStatus", error.message));
});

byId("contractQuery").addEventListener("keydown", (event) => {
  if (event.key === "Enter") {
    loadContracts().catch((error) => setStatus("contractStatus", error.message));
  }
});

byId("kindFilter").addEventListener("change", () => {
  loadContracts().catch((error) => setStatus("contractStatus", error.message));
});

byId("historyLimit").addEventListener("keydown", (event) => {
  if (event.key === "Enter") {
    const current = byId("historyTitle").textContent.replace(/ 历史$/, "").trim();
    if (current && current !== "历史") {
      loadHistory(current).catch((error) => setStatus("historyStatus", error.message));
    }
  }
});

byId("runBacktest").addEventListener("click", () => {
  runBacktest().catch((error) => setStatus("backtestStatus", error.message));
});

loadContracts().catch((error) => setStatus("contractStatus", error.message));