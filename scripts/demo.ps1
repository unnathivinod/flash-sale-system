# Full before/after demo on Windows (PowerShell).
# Usage, from the project folder:   .\scripts\demo.ps1
# If Windows blocks scripts, run once:  Set-ExecutionPolicy -Scope CurrentUser RemoteSigned

Write-Host "`n[1/3] Starting everything (first time takes a few minutes)..." -ForegroundColor Cyan
docker compose up --build -d
Start-Sleep -Seconds 8

Write-Host "`n[2/3] NAIVE design under load (expect MORE than 500 sold)..." -ForegroundColor Yellow
docker run --rm --network flashsale-net -v "${PWD}/loadtest:/scripts" grafana/k6 run --quiet /scripts/naive.js

Write-Host "`n[3/3] FIXED design under load (expect EXACTLY 500 sold)..." -ForegroundColor Green
docker run --rm --network flashsale-net -v "${PWD}/loadtest:/scripts" -e RESET=0 grafana/k6 run /scripts/flash-sale.js

Write-Host "`nOpen http://localhost:8080/stats.html to see both results side by side." -ForegroundColor Cyan
