const fmt = new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 4 });
const money = new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 2 });

let straddleContracts = [];

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

function switchPage(pageId) {
  document.querySelectorAll(".page-panel").forEach((panel) => {
    panel.classList.toggle("active", panel.id === pageId);
  });

  document.querySelectorAll("#topNavTabs .nav-link").forEach((button) => {
    button.classList.toggle("active", button.dataset.page === pageId);
  });
}

function populateSelect(id, values, placeholder, selectedValue = "") {
  const select = byId(id);
  const options = [`<option value="">${esc(placeholder)}</option>`].concat(
    values.map((value) => `<option value="${esc(value)}">${esc(value)}</option>`),
  );
  select.innerHTML = options.join("");
  select.disabled = values.length === 0;
  if (selectedValue && values.includes(selectedValue)) {
    select.value = selectedValue;
  }
}

function populateFixedSelect(id, value) {
  const select = byId(id);
  select.innerHTML = `<option value="${esc(value)}">${esc(value)}</option>`;
  select.value = value;
  select.disabled = true;
}

function uniqueValues(values, numeric = false) {
  const items = [...new Set(values.filter(Boolean))];
  return items.sort((left, right) => {
    if (numeric) {
      const numericDiff = Number(left) - Number(right);
      if (numericDiff !== 0) {
        return numericDiff;
      }
    }
    return String(left).localeCompare(String(right), "zh-CN", { numeric: true });
  });
}

function resetStraddleResults() {
  byId("straddleSummary").innerHTML = "";
  byId("straddleRows").innerHTML = "";
  setStatus("straddleBacktestStatus", "");
}

function resetStraddleSelector(prefix) {
  populateFixedSelect(`${prefix}OptionType`, prefix === "call" ? "C" : "P");
  populateSelect(`${prefix}Product`, [], "选择品种");
  populateSelect(`${prefix}Month`, [], "选择月份");
  populateSelect(`${prefix}Strike`, [], "选择行权价");
  byId(`${prefix}ContractCode`).textContent = "先加载可选合约";
}

function selectedStraddleContract(prefix) {
  const optionType = prefix === "call" ? "C" : "P";
  const product = byId(`${prefix}Product`).value;
  const month = byId(`${prefix}Month`).value;
  const strike = byId(`${prefix}Strike`).value;
  if (!product || !month || !strike) {
    return null;
  }
  return straddleContracts.find((contract) => (
    contract.option_type === optionType
    && contract.product === product
    && contract.month === month
    && contract.strike === strike
  )) || null;
}

function renderStraddleSelectorState(prefix) {
  const selected = selectedStraddleContract(prefix);
  byId(`${prefix}ContractCode`).textContent = selected
    ? `${selected.contract_code} | 可交易区间 ${selected.first_date} 至 ${selected.last_date}`
    : "未选择合约";
}

function syncStraddleSelector(prefix) {
  const optionType = prefix === "call" ? "C" : "P";
  const source = straddleContracts.filter((contract) => contract.option_type === optionType);
  populateFixedSelect(`${prefix}OptionType`, optionType);

  const currentProduct = byId(`${prefix}Product`).value;
  const currentMonth = byId(`${prefix}Month`).value;
  const currentStrike = byId(`${prefix}Strike`).value;

  const products = uniqueValues(source.map((contract) => contract.product));
  populateSelect(`${prefix}Product`, products, "选择品种", currentProduct);

  const product = byId(`${prefix}Product`).value;
  const months = product
    ? uniqueValues(source.filter((contract) => contract.product === product).map((contract) => contract.month))
    : [];
  populateSelect(`${prefix}Month`, months, "选择月份", currentMonth);

  const month = byId(`${prefix}Month`).value;
  const strikes = product && month
    ? uniqueValues(
      source
        .filter((contract) => contract.product === product && contract.month === month)
        .map((contract) => contract.strike),
      true,
    )
    : [];
  populateSelect(`${prefix}Strike`, strikes, "选择行权价", currentStrike);

  renderStraddleSelectorState(prefix);
}

function initStraddleSelectors() {
  resetStraddleSelector("call");
  resetStraddleSelector("put");
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

async function loadStraddleContracts() {
  const start = byId("straddleStartDate").value;
  const end = byId("straddleEndDate").value;
  if (!start || !end) {
    throw new Error("请先选择起止日期");
  }
  if (end < start) {
    throw new Error("结束日期不能早于开始日期");
  }

  setStatus("straddleContractStatus", "加载可选合约中...");
  resetStraddleResults();

  const data = await api(`/api/straddle/contracts?start=${encodeURIComponent(start)}&end=${encodeURIComponent(end)}`);
  straddleContracts = data.contracts || [];
  initStraddleSelectors();

  if (straddleContracts.length === 0) {
    setStatus("straddleContractStatus", "所选区间无可覆盖全程的期权合约");
    return;
  }

  syncStraddleSelector("call");
  syncStraddleSelector("put");
  setStatus("straddleContractStatus", `已加载 ${data.count} 个可选合约。按 品种 -> 月份 -> C/P -> 行权价 选择。`);
}

async function runStraddleBacktest() {
  const start = byId("straddleStartDate").value;
  const end = byId("straddleEndDate").value;
  const callContract = selectedStraddleContract("call");
  const putContract = selectedStraddleContract("put");
  const callQty = byId("callQuantity").value || "1";
  const putQty = byId("putQuantity").value || "1";

  if (!start || !end) {
    throw new Error("请先选择起止日期");
  }
  if (!callContract || !putContract) {
    throw new Error("请分别选定 CALL 和 PUT 合约");
  }
  if (Number(callQty) <= 0 || Number(putQty) <= 0) {
    throw new Error("CALL/PUT 张数必须大于 0");
  }

  setStatus("straddleBacktestStatus", "运行中...");
  const params = new URLSearchParams({
    start,
    end,
    call_contract: callContract.contract_code,
    put_contract: putContract.contract_code,
    call_qty: callQty,
    put_qty: putQty,
  });
  const data = await api(`/api/straddle/backtest?${params.toString()}`);

  byId("straddleSummary").innerHTML = [
    ["策略收益", money.format(data.total_profit)],
    ["初始成本", money.format(data.initial_cost)],
    ["期末市值", money.format(data.final_value)],
    ["交易日数", data.trading_days],
    ["CALL 合约", data.call_contract],
    ["PUT 合约", data.put_contract],
  ].map(([title, value]) => renderMetric(title, value)).join("");

  byId("straddleRows").innerHTML = data.rows.map((row) => [
    "<tr>",
    `  <td>${esc(row.date)}</td>`,
    `  <td class="text-end">${money.format(row.call_close)}</td>`,
    `  <td class="text-end">${money.format(row.put_close)}</td>`,
    `  <td class="text-end">${money.format(row.call_value)}</td>`,
    `  <td class="text-end">${money.format(row.put_value)}</td>`,
    `  <td class="text-end">${money.format(row.total_value)}</td>`,
    `  <td class="text-end">${money.format(row.total_profit)}</td>`,
    "</tr>",
  ].join("")).join("");

  setStatus("straddleBacktestStatus", `${data.actual_start_date} 到 ${data.actual_end_date} | ${data.calculation_note}`);
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

byId("loadStraddleContracts").addEventListener("click", () => {
  loadStraddleContracts().catch((error) => setStatus("straddleContractStatus", error.message));
});

byId("runStraddleBacktest").addEventListener("click", () => {
  runStraddleBacktest().catch((error) => setStatus("straddleBacktestStatus", error.message));
});

["call", "put"].forEach((prefix) => {
  ["Product", "Month", "Strike"].forEach((field) => {
    byId(`${prefix}${field}`).addEventListener("change", () => syncStraddleSelector(prefix));
  });
});

document.querySelectorAll("#topNavTabs .nav-link").forEach((button) => {
  button.addEventListener("click", () => switchPage(button.dataset.page));
});

initStraddleSelectors();
loadContracts().catch((error) => setStatus("contractStatus", error.message));