# Start with custom session balance
# Example: .\start-custom.ps1 -Balance 2000

param(
    [Parameter(Mandatory=$false)]
    [double]$Balance = 2000.0
)

.\start.ps1 -Balance $Balance
