#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Download and normalize public China option daily data.

Covered public sources:
- CFFEX monthly history zips or already extracted files: daily OHLC + Delta.
- SSE risk indicator endpoint: daily Greeks and implied volatility, no OHLC.
- SHFE daily option endpoint: daily OHLC + Delta.
- CZCE daily option endpoint: daily OHLC + Delta + implied volatility.
- GFEX daily option endpoint: daily OHLC + Delta + implied volatility.

Public exchange endpoints do not provide full historical 5-minute Greeks.
DCE and SZSE are intentionally not treated as covered here because their public
endpoints are blocked or do not expose per-contract historical records from this
environment without another data source.
"""

from __future__ import annotations

import argparse
import csv
import datetime as dt
import gzip
import io
import json
import os
import re
import sys
import time
import zipfile
from pathlib import Path
from typing import Callable, Protocol
from urllib.parse import urljoin

import requests


DAY = "%Y-%m-%d"
COMPACT_DAY = "%Y%m%d"
PROGRESS_EVERY = 50

FIELDNAMES = [
    "date",
    "exchange",
    "product",
    "contract_code",
    "contract_name",
    "option_type",
    "strike",
    "open",
    "high",
    "low",
    "close",
    "settle",
    "prev_settle",
    "volume",
    "amount",
    "open_interest",
    "open_interest_change",
    "delta",
    "gamma",
    "theta",
    "vega",
    "rho",
    "implied_vol",
    "exercise_volume",
    "source_granularity",
    "source",
]

CFFEX_OPTION_CODE = re.compile(r"^([A-Z]{1,3})(\d{4})-([CP])-(\d+(?:\.\d+)?)$")
COMPACT_OPTION_CODE = re.compile(r"^([A-Za-z]{1,3})(\d{3,4})-?([CP])[-]?(\d+(?:\.\d+)?)$")
SSE_CONTRACT_ID = re.compile(r"^(\d{6})([CP])(\d{4})M?(\d+)$")
HREF_ZIP = re.compile(r'href="([^"]+/(\d{6})\.zip)"')

CFFEX_BASE_URL = "http://www.cffex.com.cn"
SSE_RISK_URL = "http://query.sse.com.cn/commonQuery.do"
SHFE_DAILY_URL = "https://www.shfe.com.cn/data/tradedata/option/dailydata/kx{date}.dat"
CZCE_DAILY_URL = "https://www.czce.com.cn/cn/DFSStaticFiles/Option/{year}/{date}/OptionDataDaily.txt"
GFEX_DAILY_URL = "http://www.gfex.com.cn/u/interfacesWebTiDayQuotes/loadList"

SOURCE_STARTS = {
    "cffex": "2019-12-23",
    "sse-risk": "2015-02-09",
    "shfe": "2018-09-21",
    "czce": "2017-04-19",
    "gfex": "2023-12-22",
}

SOURCE_FILES = {
    "cffex": "cffex_options_daily.csv",
    "sse-risk": "sse_options_daily_greeks.csv",
    "shfe": "shfe_options_daily.csv",
    "czce": "czce_options_daily.csv",
    "gfex": "gfex_options_daily.csv",
}

CZCE_COLUMNS = [
    "合约代码",
    "昨结算",
    "今开盘",
    "最高价",
    "最低价",
    "今收盘",
    "今结算",
    "涨跌1",
    "涨跌2",
    "成交量(手)",
    "持仓量",
    "增减量",
    "成交额(万元)",
    "DELTA",
    "隐含波动率",
    "行权量",
]

SSE_HEADERS = {
    "Accept": "*/*",
    "Accept-Encoding": "gzip, deflate",
    "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
    "Cache-Control": "no-cache",
    "Connection": "keep-alive",
    "Host": "query.sse.com.cn",
    "Pragma": "no-cache",
    "Referer": "http://www.sse.com.cn/",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/101.0.4951.67 Safari/537.36",
}

SHFE_HEADERS = {"User-Agent": "Mozilla/4.0 (compatible; MSIE 5.5; Windows NT)"}

GFEX_HEADERS = {
    "Accept": "application/json, text/javascript, */*; q=0.01",
    "Accept-Encoding": "gzip, deflate",
    "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
    "Cache-Control": "no-cache",
    "Content-Type": "application/x-www-form-urlencoded; charset=UTF-8",
    "Host": "www.gfex.com.cn",
    "Origin": "http://www.gfex.com.cn",
    "Pragma": "no-cache",
    "Proxy-Connection": "keep-alive",
    "Referer": "http://www.gfex.com.cn/gfex/rihq/hqsj_tjsj.shtml",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/108.0.0.0 Safari/537.36",
    "X-Requested-With": "XMLHttpRequest",
    "content-type": "application/x-www-form-urlencoded",
}


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--sources",
        default="cffex",
        help="comma-separated: cffex,sse-risk,shfe,czce,gfex,all-public",
    )
    parser.add_argument("--start-date", default="", help="YYYY-MM-DD; source launch date used when empty")
    parser.add_argument("--end-date", default="2026-04-30", help="YYYY-MM-DD")
    parser.add_argument("--out-dir", default="data/china_options", help="output directory")
    parser.add_argument("--cffex-html", default="data.html", help="CFFEX history download page saved as HTML")
    parser.add_argument("--cffex-download-dir", default="downloads", help="CFFEX zip cache directory")
    parser.add_argument("--cffex-extract-dir", default="extracted", help="CFFEX extracted CSV directory")
    parser.add_argument("--base-url", default=CFFEX_BASE_URL, help="CFFEX base URL")
    parser.add_argument("--overwrite", action="store_true", help="replace existing output files")
    parser.add_argument("--keep-existing", action="store_true", help="with --overwrite, only replace touched outputs")
    parser.add_argument("--gzip", action="store_true", help="write .csv.gz outputs")
    parser.add_argument("--partition-year", action="store_true", help="write one output file per source/year")
    parser.add_argument("--max-dates", type=int, default=0, help="debug cap per source; 0 means no cap")
    parser.add_argument("--request-delay", type=float, default=0.0, help="seconds between network dates")
    parser.add_argument("--progress-every", type=int, default=50, help="print progress every N dates/files")
    return parser.parse_args()


def main() -> int:
    global PROGRESS_EVERY
    args = parse_args()
    PROGRESS_EVERY = max(1, args.progress_every)
    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)

    end_date = parse_day(args.end_date)
    requested_sources = expand_sources(args.sources)
    manifest_path = out_dir / "download_manifest.json"
    manifest = load_manifest(manifest_path)
    manifest["end_date"] = args.end_date
    manifest.setdefault("sources", {})
    manifest["unsupported"] = unsupported_notes()

    session = requests.Session()
    for source in requested_sources:
        source_start = parse_day(args.start_date or SOURCE_STARTS[source])
        source_end = end_date
        if source_start > source_end:
            manifest["sources"][source] = {"rows": 0, "reason": "start after end"}
            continue

        existing_paths = existing_output_paths(out_dir, source, args.gzip, args.partition_year)
        if existing_paths and not args.overwrite:
            manifest["sources"][source] = {
                "rows": None,
                "outputs": [str(output_path) for output_path in existing_paths],
                "skipped": "exists",
            }
            continue

        if not args.keep_existing:
            remove_stale_outputs(out_dir, source, args.gzip, args.partition_year)
        writer = make_writer(out_dir, source, args.gzip, args.partition_year)

        try:
            if source == "cffex":
                summary = write_cffex(args, writer, source_start, source_end, session)
            else:
                dates = trading_dates(Path(args.cffex_extract_dir), source_start, source_end)
                if args.max_dates > 0:
                    dates = dates[: args.max_dates]
                downloader = public_downloaders()[source]
                summary = downloader(session, dates, writer, args.request_delay)
        finally:
            writer.close()
        summary["outputs"] = writer.outputs()
        manifest["sources"][source] = summary

    manifest_path.write_text(json.dumps(manifest, indent=2, ensure_ascii=False), encoding="utf-8")
    print(json.dumps(manifest, indent=2, ensure_ascii=False))
    return 0


def expand_sources(raw_sources: str) -> list[str]:
    parts = [part.strip() for part in raw_sources.split(",") if part.strip()]
    if parts == ["all-public"]:
        return ["cffex", "sse-risk", "shfe", "czce", "gfex"]
    valid = set(SOURCE_FILES)
    unknown = [part for part in parts if part not in valid]
    if unknown:
        raise SystemExit(f"unknown source(s): {', '.join(unknown)}")
    return parts


def load_manifest(manifest_path: Path) -> dict[str, object]:
    if not manifest_path.exists():
        return {"sources": {}}
    try:
        existing = json.loads(manifest_path.read_text(encoding="utf-8"))
    except json.JSONDecodeError:
        return {"sources": {}}
    if not isinstance(existing, dict):
        return {"sources": {}}
    if not isinstance(existing.get("sources"), dict):
        existing["sources"] = {}
    return existing


def unsupported_notes() -> dict[str, str]:
    return {
        "dce": "DCE JSON endpoint returns anti-bot HTML/HTTP 412 from this environment.",
        "szse": "Public AkShare/SZSE endpoints here expose current/day summary, not full historical per-contract OHLC Greeks.",
        "intraday_greeks": "No public exchange source found for full historical 5-minute Greeks; use vendor data or compute from intraday prices.",
    }


def parse_day(raw_day: str) -> dt.date:
    return dt.datetime.strptime(raw_day, DAY).date()


def compact_day(day: dt.date) -> str:
    return day.strftime(COMPACT_DAY)


def write_cffex(
    args: argparse.Namespace,
    writer: "BaseWriter",
    start_date: dt.date,
    end_date: dt.date,
    session: requests.Session,
) -> dict[str, object]:
    html_path = Path(args.cffex_html)
    download_dir = Path(args.cffex_download_dir)
    extract_dir = Path(args.cffex_extract_dir)
    ensure_cffex_archives(html_path, download_dir, extract_dir, start_date, end_date, args.base_url, session)

    products: dict[str, dict[str, str | int]] = {}
    scanned_files = 0
    csv_paths = sorted(extract_dir.glob("*/*.csv"))
    for index, csv_path in enumerate(csv_paths, start=1):
        file_date = date_from_csv_name(csv_path.name)
        if file_date is None or file_date < start_date or file_date > end_date:
            continue
        progress("cffex", index, len(csv_paths), file_date)
        scanned_files += 1
        with csv_path.open("r", encoding="gb18030", errors="replace", newline="") as csv_file:
            reader = csv.reader(csv_file)
            next(reader, None)
            for row in reader:
                normalized = normalize_cffex_row(file_date, row)
                if normalized is None:
                    continue
                writer.write(normalized)
                product = normalized["product"]
                product_summary = products.setdefault(
                    product,
                    {"rows": 0, "first_date": normalized["date"], "last_date": normalized["date"]},
                )
                product_summary["rows"] = int(product_summary["rows"]) + 1
                product_summary["first_date"] = min(str(product_summary["first_date"]), normalized["date"])
                product_summary["last_date"] = max(str(product_summary["last_date"]), normalized["date"])

    return {"rows": writer.rows, "files": scanned_files, "products": products}


def ensure_cffex_archives(
    html_path: Path,
    download_dir: Path,
    extract_dir: Path,
    start_date: dt.date,
    end_date: dt.date,
    base_url: str,
    session: requests.Session,
) -> None:
    if not html_path.exists():
        return
    download_dir.mkdir(parents=True, exist_ok=True)
    extract_dir.mkdir(parents=True, exist_ok=True)
    html = html_path.read_text(encoding="utf-8", errors="replace")
    start_month = start_date.strftime("%Y%m")
    end_month = end_date.strftime("%Y%m")
    for relative_url, month in HREF_ZIP.findall(html):
        if month < start_month or month > end_month:
            continue
        zip_url = urljoin(base_url.rstrip("/") + "/", relative_url.lstrip("/"))
        zip_path = download_dir / f"{month}.zip"
        target_dir = extract_dir / month
        if not zip_path.exists() or zip_path.stat().st_size == 0:
            response = session.get(zip_url, timeout=60)
            response.raise_for_status()
            zip_path.write_bytes(response.content)
        if not target_dir.exists() or not any(target_dir.glob("*.csv")):
            target_dir.mkdir(parents=True, exist_ok=True)
            with zipfile.ZipFile(zip_path) as archive:
                archive.extractall(target_dir)


def normalize_cffex_row(file_date: dt.date, row: list[str]) -> dict[str, str] | None:
    if len(row) < 14:
        return None
    contract_code = clean(row[0]).lstrip("\ufeff")
    match = CFFEX_OPTION_CODE.match(contract_code)
    if match is None:
        return None
    product, _, option_type, strike = match.groups()
    normalized = empty_row(file_date, "CFFEX", product, contract_code)
    normalized.update(
        {
            "option_type": option_type,
            "strike": strike,
            "open": clean(row[1]),
            "high": clean(row[2]),
            "low": clean(row[3]),
            "volume": clean(row[4]),
            "amount": clean(row[5]),
            "open_interest": clean(row[6]),
            "open_interest_change": clean(row[7]),
            "close": clean(row[8]),
            "settle": clean(row[9]),
            "prev_settle": clean(row[10]),
            "delta": clean(row[13]),
            "source_granularity": "1d",
            "source": "cffex_history_zip",
        }
    )
    return normalized


def public_downloaders() -> dict[str, Callable[[requests.Session, list[dt.date], "BaseWriter", float], dict[str, object]]]:
    return {
        "sse-risk": download_sse_risk,
        "shfe": download_shfe,
        "czce": download_czce,
        "gfex": download_gfex,
    }


def download_sse_risk(
    session: requests.Session,
    dates: list[dt.date],
    writer: "BaseWriter",
    request_delay: float,
) -> dict[str, object]:
    errors: list[str] = []
    for index, trade_date in enumerate(dates, start=1):
        progress("sse-risk", index, len(dates), trade_date)
        params = {
            "isPagination": "false",
            "trade_date": compact_day(trade_date),
            "sqlId": "SSE_ZQPZ_YSP_GGQQZSXT_YSHQ_QQFXZB_DATE_L",
            "contractSymbol": "",
        }
        try:
            response = session.get(SSE_RISK_URL, params=params, headers=SSE_HEADERS, timeout=30)
            response.raise_for_status()
            payload = response.json()
            records = payload.get("result") or []
        except Exception as error:  # noqa: BLE001
            errors.append(f"{trade_date}: {error}")
            continue
        for record in records:
            contract_id = clean(record.get("CONTRACT_ID"))
            option_type, strike = parse_sse_contract(contract_id)
            normalized = empty_row(trade_date, "SSE", "", clean(record.get("SECURITY_ID")))
            normalized.update(
                {
                    "contract_name": clean(record.get("CONTRACT_SYMBOL")),
                    "option_type": option_type,
                    "strike": strike,
                    "delta": clean(record.get("DELTA_VALUE")),
                    "theta": clean(record.get("THETA_VALUE")),
                    "gamma": clean(record.get("GAMMA_VALUE")),
                    "vega": clean(record.get("VEGA_VALUE")),
                    "rho": clean(record.get("RHO_VALUE")),
                    "implied_vol": clean(record.get("IMPLC_VOLATLTY")),
                    "source_granularity": "1d_greeks_only",
                    "source": "sse_risk_indicator",
                }
            )
            writer.write(normalized)
        delay(request_delay)
    return {"rows": writer.rows, "dates": len(dates), "errors": errors[:20], "error_count": len(errors)}


def download_shfe(
    session: requests.Session,
    dates: list[dt.date],
    writer: "BaseWriter",
    request_delay: float,
) -> dict[str, object]:
    errors: list[str] = []
    for index, trade_date in enumerate(dates, start=1):
        progress("shfe", index, len(dates), trade_date)
        try:
            response = session.get(SHFE_DAILY_URL.format(date=compact_day(trade_date)), headers=SHFE_HEADERS, timeout=30)
            if response.status_code == 404:
                continue
            response.raise_for_status()
            payload = response.json()
            records = payload.get("o_curinstrument") or []
        except Exception as error:  # noqa: BLE001
            errors.append(f"{trade_date}: {error}")
            continue
        for record in records:
            contract_code = clean(record.get("INSTRUMENTID"))
            if not contract_code or contract_code in {"小计", "合计"}:
                continue
            product = clean(record.get("PRODUCTNAME"))
            option_type = {"1": "C", "2": "P"}.get(clean(record.get("OPTIONSTYPE")), "")
            normalized = empty_row(trade_date, "SHFE", product, contract_code)
            normalized.update(
                {
                    "option_type": option_type,
                    "strike": clean(record.get("STRIKEPRICE")),
                    "open": clean(record.get("OPENPRICE")),
                    "high": clean(record.get("HIGHESTPRICE")),
                    "low": clean(record.get("LOWESTPRICE")),
                    "close": clean(record.get("CLOSEPRICE")),
                    "settle": clean(record.get("SETTLEMENTPRICE")),
                    "prev_settle": clean(record.get("PRESETTLEMENTPRICE")),
                    "volume": clean(record.get("VOLUME")),
                    "amount": clean(record.get("TURNOVER")),
                    "open_interest": clean(record.get("OPENINTEREST")),
                    "open_interest_change": clean(record.get("OPENINTERESTCHG")),
                    "delta": clean(record.get("DELTA")),
                    "exercise_volume": clean(record.get("EXECVOLUME")),
                    "source_granularity": "1d",
                    "source": "shfe_option_daily",
                }
            )
            writer.write(normalized)
        delay(request_delay)
    return {"rows": writer.rows, "dates": len(dates), "errors": errors[:20], "error_count": len(errors)}


def download_czce(
    session: requests.Session,
    dates: list[dt.date],
    writer: "BaseWriter",
    request_delay: float,
) -> dict[str, object]:
    errors: list[str] = []
    for index, trade_date in enumerate(dates, start=1):
        progress("czce", index, len(dates), trade_date)
        url = CZCE_DAILY_URL.format(year=trade_date.strftime("%Y"), date=compact_day(trade_date))
        try:
            response = session.get(url, timeout=30)
            if response.status_code == 404:
                continue
            response.raise_for_status()
            if "<html" in response.text[:200].lower():
                continue
            records = parse_czce_text(response.text)
        except Exception as error:  # noqa: BLE001
            errors.append(f"{trade_date}: {error}")
            continue
        for record in records:
            contract_code = clean(record.get("合约代码"))
            option_type, strike = parse_compact_option(contract_code)
            normalized = empty_row(trade_date, "CZCE", parse_product_prefix(contract_code), contract_code)
            normalized.update(
                {
                    "option_type": option_type,
                    "strike": strike,
                    "open": clean(record.get("今开盘")),
                    "high": clean(record.get("最高价")),
                    "low": clean(record.get("最低价")),
                    "close": clean(record.get("今收盘")),
                    "settle": clean(record.get("今结算")),
                    "prev_settle": clean(record.get("昨结算")),
                    "volume": clean(record.get("成交量(手)")),
                    "amount": clean(record.get("成交额(万元)")),
                    "open_interest": clean(record.get("持仓量")),
                    "open_interest_change": clean(record.get("增减量")),
                    "delta": clean(record.get("DELTA")),
                    "implied_vol": clean(record.get("隐含波动率")),
                    "exercise_volume": clean(record.get("行权量")),
                    "source_granularity": "1d",
                    "source": "czce_option_daily",
                }
            )
            writer.write(normalized)
        delay(request_delay)
    return {"rows": writer.rows, "dates": len(dates), "errors": errors[:20], "error_count": len(errors)}


def download_gfex(
    session: requests.Session,
    dates: list[dt.date],
    writer: "BaseWriter",
    request_delay: float,
) -> dict[str, object]:
    errors: list[str] = []
    for index, trade_date in enumerate(dates, start=1):
        progress("gfex", index, len(dates), trade_date)
        try:
            response = session.post(
                GFEX_DAILY_URL,
                data={"trade_date": compact_day(trade_date), "trade_type": "1"},
                headers=GFEX_HEADERS,
                timeout=30,
            )
            response.raise_for_status()
            payload = response.json()
            records = payload.get("data") or []
        except Exception as error:  # noqa: BLE001
            errors.append(f"{trade_date}: {error}")
            continue
        for record in records:
            contract_code = clean(record.get("delivMonth"))
            if not contract_code:
                continue
            option_type, strike = parse_compact_option(contract_code)
            normalized = empty_row(trade_date, "GFEX", clean(record.get("variety")), contract_code)
            normalized.update(
                {
                    "option_type": option_type,
                    "strike": strike,
                    "open": clean(record.get("open")),
                    "high": clean(record.get("high")),
                    "low": clean(record.get("low")),
                    "close": clean(record.get("close")),
                    "settle": clean(record.get("clearPrice")),
                    "prev_settle": clean(record.get("lastClear")),
                    "volume": clean(record.get("volumn")),
                    "amount": clean(record.get("turnover")),
                    "open_interest": clean(record.get("openInterest")),
                    "open_interest_change": clean(record.get("diffI")),
                    "delta": clean(record.get("delta")),
                    "implied_vol": clean(record.get("impliedVolatility")),
                    "exercise_volume": clean(record.get("matchQtySum")),
                    "source_granularity": "1d",
                    "source": "gfex_option_daily",
                }
            )
            writer.write(normalized)
        delay(request_delay)
    return {"rows": writer.rows, "dates": len(dates), "errors": errors[:20], "error_count": len(errors)}


def parse_czce_text(text: str) -> list[dict[str, str]]:
    lines = [line for line in text.splitlines() if line.strip()]
    if len(lines) < 3:
        return []
    reader = csv.reader(io.StringIO("\n".join(lines[2:])), delimiter="|")
    records: list[dict[str, str]] = []
    for fields in reader:
        if len(fields) < len(CZCE_COLUMNS):
            continue
        cleaned_record = {column: clean(fields[index]) for index, column in enumerate(CZCE_COLUMNS)}
        contract_code = cleaned_record.get("合约代码", "")
        if COMPACT_OPTION_CODE.match(contract_code):
            records.append(cleaned_record)
    return records


def trading_dates(extract_dir: Path, start_date: dt.date, end_date: dt.date) -> list[dt.date]:
    dates: set[dt.date] = set()
    if extract_dir.exists():
        for csv_path in extract_dir.glob("*/*.csv"):
            file_date = date_from_csv_name(csv_path.name)
            if file_date is not None and start_date <= file_date <= end_date:
                dates.add(file_date)
    if not dates:
        current = start_date
        while current <= end_date:
            if current.weekday() < 5:
                dates.add(current)
            current += dt.timedelta(days=1)
    return sorted(dates)


def date_from_csv_name(name: str) -> dt.date | None:
    if len(name) < 8 or not name[:8].isdigit():
        return None
    return dt.datetime.strptime(name[:8], COMPACT_DAY).date()


def empty_row(trade_date: dt.date, exchange: str, product: str, contract_code: str) -> dict[str, str]:
    row = {field: "" for field in FIELDNAMES}
    row.update(
        {
            "date": trade_date.strftime(DAY),
            "exchange": exchange,
            "product": product,
            "contract_code": contract_code,
        }
    )
    return row


def clean(value: object) -> str:
    if value is None:
        return ""
    text = str(value).replace("\ufeff", "").strip()
    if text.lower() in {"", "nan", "none", "null", "--", "-"}:
        return ""
    return text.replace(",", "")


def parse_compact_option(contract_code: str) -> tuple[str, str]:
    match = COMPACT_OPTION_CODE.match(contract_code)
    if match is None:
        return "", ""
    return match.group(3), match.group(4)


def parse_product_prefix(contract_code: str) -> str:
    match = re.match(r"^([A-Za-z]+)", contract_code)
    return match.group(1).upper() if match else ""


def parse_sse_contract(contract_id: str) -> tuple[str, str]:
    match = SSE_CONTRACT_ID.match(contract_id)
    if match is None:
        return "", ""
    strike = match.group(4)
    if len(strike) > 3:
        strike = f"{int(strike) / 1000:g}"
    return match.group(2), strike


def delay(seconds: float) -> None:
    if seconds > 0:
        time.sleep(seconds)


def progress(source: str, index: int, total: int, trade_date: dt.date) -> None:
    if index == 1 or index == total or index % PROGRESS_EVERY == 0:
        print(f"{source}: {index}/{total} {trade_date}", file=sys.stderr, flush=True)


def existing_output_paths(out_dir: Path, source: str, gzip_enabled: bool, partition_year: bool) -> list[Path]:
    if partition_year:
        return sorted(out_dir.glob(partition_pattern(source, gzip_enabled)))
    output_path = source_output_path(out_dir, source, gzip_enabled)
    return [output_path] if output_path.exists() else []


def remove_stale_outputs(out_dir: Path, source: str, gzip_enabled: bool, partition_year: bool) -> None:
    del gzip_enabled, partition_year
    for path in all_source_output_paths(out_dir, source):
        path.unlink()


def all_source_output_paths(out_dir: Path, source: str) -> list[Path]:
    base_name = SOURCE_FILES[source]
    stem = Path(base_name).stem
    candidates = [out_dir / base_name, out_dir / f"{base_name}.gz"]
    candidates.extend(out_dir.glob(f"{stem}_*.csv"))
    candidates.extend(out_dir.glob(f"{stem}_*.csv.gz"))
    candidates.extend(out_dir.glob(f"{stem}_*.csv.tmp"))
    candidates.extend(out_dir.glob(f"{stem}_*.csv.gz.tmp"))
    candidates.extend(out_dir.glob(f"{base_name}.tmp"))
    candidates.extend(out_dir.glob(f"{base_name}.gz.tmp"))
    seen: set[Path] = set()
    existing: list[Path] = []
    for path in candidates:
        if path.exists() and path not in seen:
            seen.add(path)
            existing.append(path)
    return sorted(existing)


def make_writer(out_dir: Path, source: str, gzip_enabled: bool, partition_year: bool) -> "BaseWriter":
    if partition_year:
        return PartitionedSourceWriter(out_dir, source, gzip_enabled)
    return SourceWriter(source_output_path(out_dir, source, gzip_enabled))


def source_output_path(out_dir: Path, source: str, gzip_enabled: bool) -> Path:
    file_name = SOURCE_FILES[source]
    if gzip_enabled:
        file_name = f"{file_name}.gz"
    return out_dir / file_name


def partition_output_path(out_dir: Path, source: str, year: str, gzip_enabled: bool) -> Path:
    base_name = SOURCE_FILES[source]
    stem = Path(base_name).stem
    extension = ".csv.gz" if gzip_enabled else ".csv"
    return out_dir / f"{stem}_{year}{extension}"


def partition_pattern(source: str, gzip_enabled: bool) -> str:
    stem = Path(SOURCE_FILES[source]).stem
    extension = ".csv.gz" if gzip_enabled else ".csv"
    return f"{stem}_*{extension}"


class BaseWriter(Protocol):
    rows: int

    def write(self, row: dict[str, str]) -> None:
        ...

    def close(self) -> None:
        ...

    def outputs(self) -> list[str]:
        ...


class SourceWriter:
    def __init__(self, output_path: Path):
        output_path.parent.mkdir(parents=True, exist_ok=True)
        self.output_path = output_path
        self.temp_path = output_path.with_suffix(output_path.suffix + ".tmp")
        if output_path.suffix == ".gz":
            self.file_handle = gzip.open(self.temp_path, "wt", encoding="utf-8", newline="")
        else:
            self.file_handle = self.temp_path.open("w", encoding="utf-8", newline="")
        self.writer = csv.DictWriter(self.file_handle, fieldnames=FIELDNAMES)
        self.writer.writeheader()
        self.rows = 0
        self.closed = False

    def write(self, row: dict[str, str]) -> None:
        self.writer.writerow({field: row.get(field, "") for field in FIELDNAMES})
        self.rows += 1

    def close(self) -> None:
        if self.closed:
            return
        self.file_handle.close()
        if self.temp_path.exists():
            os.replace(self.temp_path, self.output_path)
        elif not self.output_path.exists():
            raise FileNotFoundError(f"missing output temp file: {self.temp_path}")
        self.closed = True

    def outputs(self) -> list[str]:
        return [str(self.output_path)]


class PartitionedSourceWriter:
    def __init__(self, out_dir: Path, source: str, gzip_enabled: bool):
        out_dir.mkdir(parents=True, exist_ok=True)
        self.out_dir = out_dir
        self.source = source
        self.gzip_enabled = gzip_enabled
        self.writers: dict[str, SourceWriter] = {}
        self.rows = 0
        self.closed = False

    def write(self, row: dict[str, str]) -> None:
        year = (row.get("date") or "unknown")[:4]
        writer = self.writers.get(year)
        if writer is None:
            writer = SourceWriter(partition_output_path(self.out_dir, self.source, year, self.gzip_enabled))
            self.writers[year] = writer
        writer.write(row)
        self.rows += 1

    def close(self) -> None:
        if self.closed:
            return
        for writer in self.writers.values():
            writer.close()
        self.closed = True

    def outputs(self) -> list[str]:
        return [str(self.writers[year].output_path) for year in sorted(self.writers)]


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        print("interrupted", file=sys.stderr)
        raise SystemExit(130)