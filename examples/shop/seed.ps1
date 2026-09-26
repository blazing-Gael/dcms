<#
  Seed the shop demo with 30+ products across a few categories, so the storefront's
  "Load more" pagination has something to page through. Authenticates as the admin
  through the panel login (no API token needed) and creates everything via the public
  REST API. Products are created without images on purpose - the storefront renders a
  coloured tile with the product's initial when there's no image, which looks clean
  and avoids multipart uploads in Windows PowerShell 5.1.

  Run the server first (see README), then from this folder:
      powershell -ExecutionPolicy Bypass -File .\seed.ps1
  Optionally pass the admin credentials:
      .\seed.ps1 -Email you@shop.test -Password shoppass
#>
param(
  [string]$Email    = "you@shop.test",
  [string]$Password = "shoppass",
  [string]$BaseUrl  = "http://localhost:8080"
)
$ErrorActionPreference = "Stop"
$api = "$BaseUrl/api/v1"
$inv = [System.Globalization.CultureInfo]::InvariantCulture

# -- sign in through the admin panel to get a session cookie the API accepts ------
$sess = New-Object Microsoft.PowerShell.Commands.WebRequestSession
Invoke-WebRequest "$BaseUrl/__admin/login" -WebSession $sess -UseBasicParsing | Out-Null
$csrf = ($sess.Cookies.GetCookies([uri]"$BaseUrl/__admin") | Where-Object { $_.Name -eq "dcms_admin_csrf" }).Value
try {
  Invoke-WebRequest "$BaseUrl/__admin/login" -Method Post -WebSession $sess -UseBasicParsing `
    -Body @{ email = $Email; password = $Password; csrf = $csrf } | Out-Null
} catch {
  Write-Error "Login failed for $Email - is the server running and the admin seeded? ($_)"
  return
}

function Post-Json($collection, $obj) {
  Invoke-RestMethod "$api/$collection" -Method Post -WebSession $sess `
    -ContentType "application/json" -Body ($obj | ConvertTo-Json -Depth 6 -Compress)
}

function Ensure-Category($name, $slug) {
  try { return (Post-Json "categories" @{ name = $name; slug = $slug }).data.id }
  catch {
    $list = Invoke-RestMethod "$api/categories?limit=100" -WebSession $sess
    return ($list.data | Where-Object { $_.slug -eq $slug } | Select-Object -First 1).id
  }
}

# -- the catalog: 33 products across 5 categories --------------------------------
$catalog = @(
  @{ cat = "Coffee";      base = 12.0; names = @("House Blend 250g","Ethiopia Yirgacheffe","Colombian Supremo","Espresso Roast","Decaf Midnight","Sumatra Dark Roast","Guatemala Antigua","Kenya AA","Brazil Santos","Cold Brew Concentrate") },
  @{ cat = "Tea";         base = 8.0;  names = @("Green Sencha","Earl Grey","English Breakfast","Chamomile","Peppermint","Jasmine Pearls","Masala Chai","Oolong") },
  @{ cat = "Equipment";   base = 24.0; names = @("Pour-Over Dripper","French Press","Gooseneck Kettle","Burr Grinder","Espresso Tamper","Milk Frother","Brewing Scale") },
  @{ cat = "Accessories"; base = 9.0;  names = @("Ceramic Mug","Travel Tumbler","Reusable Filter","Storage Canister","Cleaning Tablets") },
  @{ cat = "Gifts";       base = 30.0; names = @("Coffee Sampler Box","Tea Sampler Box","Brewing Starter Kit") }
)
# stock pattern: cycles through some zeros (sold out) and lows (good for the oversell shot)
$stockPattern = @(24,4,0,18,9,30,6,12,0,15,22,3)

$idx = 0; $made = 0; $skipped = 0
foreach ($group in $catalog) {
  $catSlug = ($group.cat.ToLower() -replace '[^a-z0-9]+','-').Trim('-')
  $catId = Ensure-Category $group.cat $catSlug
  Write-Host ("- {0}" -f $group.cat) -ForegroundColor Cyan
  for ($i = 0; $i -lt $group.names.Count; $i++) {
    $name  = $group.names[$i]
    $slug  = ($name.ToLower() -replace '[^a-z0-9]+','-').Trim('-')
    $price = ([double]$group.base + ($i * 1.5)).ToString("0.00", $inv)
    $stock = $stockPattern[$idx % $stockPattern.Count]
    try {
      $p = Post-Json "products" @{ name = $name; slug = $slug; price = $price; cost = "0.00"; stock = $stock; category = $catId }
      Invoke-RestMethod "$api/products/$($p.data.id)/publish" -Method Post -WebSession $sess | Out-Null
      $tag = if ($stock -eq 0) { " (sold out)" } elseif ($stock -le 4) { " (low: $stock)" } else { "" }
      Write-Host ("    {0,-24} `${1,-6}{2}" -f $name, $price, $tag)
      $made++
    } catch {
      Write-Host ("    {0,-24} skipped (already exists?)" -f $name) -ForegroundColor DarkGray
      $skipped++
    }
    $idx++
  }
}

# -- a customer, and a staff user for the role-scoped transition demo -------------
try { Post-Json "customers" @{ name = "Ada Lovelace"; email = "ada@example.com" } | Out-Null } catch {}
try {
  Invoke-WebRequest "$BaseUrl/__admin/users" -Method Post -WebSession $sess -UseBasicParsing `
    -Body @{ email = "staff@shop.test"; password = "staffpass"; name = "Sam Staff"; roles = "staff"; csrf = $csrf } | Out-Null
} catch {}

Write-Host ""
Write-Host ("Done - {0} products created, {1} skipped." -f $made, $skipped) -ForegroundColor Green
Write-Host "Storefront:  $BaseUrl/            (browse, cart, checkout)"
Write-Host "Admin:       $BaseUrl/__admin     ($Email / $Password  |  staff@shop.test / staffpass)"
