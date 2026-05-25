# China Options Public Daily Data

This directory contains normalized public option-market data downloaded through 2026-04-30.

## Coverage

| Source | Status | Rows | Date range | Files | Granularity |
| --- | --- | ---: | --- | ---: | --- |
| CFFEX | complete public daily pull | 776,594 | 2019-12-23 to 2026-04-30 | 8 | 1d |
| SHFE | complete public daily pull | 4,169,662 | 2018-09-21 to 2026-04-30 | 9 | 1d |
| CZCE | complete public daily pull | 4,010,928 | 2017-04-19 to 2026-04-30 | 10 | 1d |
| GFEX | partial public daily pull | 52,982 | 2023-12-22 to 2024-01-26 | 2 | 1d |
| SSE risk indicators | blocked during full pull | 0 | none kept | 0 | 1d Greeks only when reachable |

## Schema

All CSV gzip files use the same columns:

```text
date,exchange,product,contract_code,contract_name,option_type,strike,open,high,low,close,settle,prev_settle,volume,amount,open_interest,open_interest_change,delta,gamma,theta,vega,rho,implied_vol,exercise_volume,source_granularity,source
```

## Notes

- The smallest stable public granularity found here is daily. Full historical 5-minute Greeks were not exposed by the public exchange endpoints tested from this environment.
- CFFEX, SHFE, and CZCE daily public pulls completed through 2026-04-30.
- GFEX began returning EdgeOne/WAF `567` responses after early 2024 during full historical pulls, so only completed shards are kept.
- SSE risk-indicator endpoint returned HTML error pages during full historical pulls, so no incomplete SSE shard is kept.
- DCE daily option endpoint returned HTTP `412` anti-bot HTML from this environment.
- SZSE public/AkShare endpoints available here expose current contracts or daily summary, not full historical per-contract OHLC/Greeks from listing.

Use `download_manifest.json` for exact file-level row counts, byte sizes, and first/last dates.

## Re-run

```sh
rtk uv run --with requests scripts/download_china_options.py --sources all-public --end-date 2026-04-30 --out-dir data/china_options --overwrite --gzip --partition-year
```
