#!/usr/bin/env python3
"""Download Zhengzhou glass futures main continuous daily OHLC data."""

from __future__ import annotations

import argparse
from pathlib import Path

import akshare as ak


REQUIRED_COLUMNS = ["date", "open", "high", "low", "close"]


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--symbol",
        default="FG0",
        help="AkShare Sina futures symbol for main continuous contract",
    )
    parser.add_argument(
        "--out",
        default="data/FG_future/fg_main_daily_ohlc.csv",
        help="output CSV path",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    output_path = Path(args.out)
    output_path.parent.mkdir(parents=True, exist_ok=True)

    dataframe = ak.futures_zh_daily_sina(symbol=args.symbol)
    missing_columns = [column for column in REQUIRED_COLUMNS if column not in dataframe.columns]
    if missing_columns:
        missing = ", ".join(missing_columns)
        raise ValueError(f"missing expected columns: {missing}")

    ohlc = dataframe.loc[:, REQUIRED_COLUMNS].copy()
    ohlc = ohlc.sort_values("date").drop_duplicates(subset=["date"], keep="last")
    ohlc.to_csv(output_path, index=False)

    start_date = ohlc.iloc[0]["date"]
    end_date = ohlc.iloc[-1]["date"]
    print(f"wrote {len(ohlc)} rows to {output_path}")
    print(f"date range: {start_date} -> {end_date}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())