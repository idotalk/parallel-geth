$headers = @{ "Content-Type" = "application/json" }
$rpcUri  = "https://docs-demo.quiknode.pro/" 

# Step 1: Fetch the latest block with full transaction objects expanded 
$blockBody = @{
    jsonrpc = "2.0"
    method  = "eth_getBlockByNumber"
    params  = @("latest", $true)
    id      = 1
} | ConvertTo-Json -Depth 10

$blockResponse = Invoke-RestMethod -Uri $rpcUri -Method Post -Headers $headers -Body $blockBody
$block        = $blockResponse.result
$transactions = $block.transactions
$blockHex     = $block.number

# Convert hex block number to standard decimal integer for the filename
$blockDecimal = [Convert]::ToInt64($blockHex, 16)
$fileName     = "$blockDecimal.json"
$outputPath = Join-Path -Path $PSScriptRoot -ChildPath "blocksdata"

# Ensure target directory exists
if (-not (Test-Path -Path $outputPath)) {
    New-Item -ItemType Directory -Path $outputPath | Out-Null
}

# Combine path and filename safely
$fullFilePath = Join-Path -Path $outputPath -ChildPath $fileName

Write-Host "Formatting block $blockDecimal into Ethereum benchmark schema..."

# Initialize an array to hold the formatted transactions matching the blockchain test schema
$formattedTransactions = @()

# Step 2: Loop through each transaction to fetch its accessList and map it
foreach ($tx in $transactions) {
    
    # Payload explicitly prepared for eth_createAccessList simulation
    $txObject = @{
        from  = $tx.from
        to    = $tx.to
        data  = $tx.input
    }
    if ($tx.value) { $txObject.value = $tx.value }
    if ($tx.gas)   { $txObject.gas   = $tx.gas }

    $accessListBody = @{
        jsonrpc = "2.0"
        method  = "eth_createAccessList"
        params  = @($txObject, $blockHex)
        id      = 1
    } | ConvertTo-Json -Depth 10

    # Default to empty accessList if call fails or returns empty
    $computedAccessList = @()
    try {
        $alResponse = Invoke-RestMethod -Uri $rpcUri -Method Post -Headers $headers -Body $accessListBody
        if ($alResponse.result.accessList) {
            $computedAccessList = $alResponse.result.accessList
        }
    } catch {
        # Fallback to empty if simulation throws error
    }

    # Format the individual transaction to match standard EIP1559/AccessList test objects
    $formattedTx = [ordered]@{
        accessList           = $computedAccessList
        data                 = $tx.input
        gasLimit             = $tx.gas
        maxFeePerGas         = $tx.maxFeePerGas
        maxPriorityFeePerGas = $tx.maxPriorityFeePerGas
        nonce                = $tx.nonce
        to                   = if ([string]::IsNullOrEmpty($tx.to)) { "" } else { $tx.to }
        value                = $tx.value
        v                    = $tx.v
        r                    = $tx.r
        s                    = $tx.s
    }

    # Clean out any empty/null fields to keep schema pure
    foreach ($key in @($formattedTx.Keys)) {
        if ($null -eq $formattedTx[$key]) { $formattedTx.Remove($key) }
    }

    $formattedTransactions += $formattedTx
}

# Step 3: Construct the exact top-level blockchain test wrapper schema
$testSchemaOutput = [ordered]@{
    # Test name key wrapper at root level
    "bcLiveGeneratedBlock_$blockDecimal" = [ordered]@{
        blockHeader = [ordered]@{
            baseFeePerGas        = $block.baseFeePerGas
            bloom                = $block.logsBloom
            coinbase             = $block.miner
            difficulty           = $block.difficulty
            extraData            = $block.extraData
            gasLimit             = $block.gasLimit
            gasUsed              = $block.gasUsed
            mixHash              = $block.mixHash
            nonce                = $block.nonce
            number               = $block.number
            parentHash           = $block.parentHash
            receiptTrie          = $block.receiptsRoot
            stateRoot            = $block.stateRoot
            timestamp            = $block.timestamp
            transactionTrie      = $block.transactionsRoot
            uncleHash            = $block.sha3Uncles
        }
        blocks = @(
            [ordered]@{
                blockHeader = [ordered]@{
                    baseFeePerGas        = $block.baseFeePerGas
                    bloom                = $block.logsBloom
                    coinbase             = $block.miner
                    difficulty           = $block.difficulty
                    extraData            = $block.extraData
                    gasLimit             = $block.gasLimit
                    gasUsed              = $block.gasUsed
                    mixHash              = $block.mixHash
                    nonce                = $block.nonce
                    number               = $block.number
                    parentHash           = $block.parentHash
                    receiptTrie          = $block.receiptsRoot
                    stateRoot            = $block.stateRoot
                    timestamp            = $block.timestamp
                    transactionTrie      = $block.transactionsRoot
                    uncleHash            = $block.sha3Uncles
                }
                transactions = $formattedTransactions
                uncleHeaders = @()
            }
        )
    }
}

# Clean out any null properties from the top-level blockheaders
foreach ($header in @($testSchemaOutput."bcLiveGeneratedBlock_$blockDecimal".blockHeader, $testSchemaOutput."bcLiveGeneratedBlock_$blockDecimal".blocks[0].blockHeader)) {
    foreach ($key in @($header.Keys)) {
        if ($null -eq $header[$key]) { $header.Remove($key) }
    }
}

# Write structure to JSON file
$testSchemaOutput | ConvertTo-Json -Depth 10 | Out-File -FilePath $fullFilePath -Encoding utf8
Write-Host "Success! File written to: $fullFilePath" -ForegroundColor Green