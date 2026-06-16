#!/usr/bin/env node
// Grants.gov MCP Server
// Wraps the public Grants.gov REST API (no auth required).
// Implements MCP over stdio — no npm dependencies.

'use strict';
const https = require('https');

// ---- stdio JSON-RPC transport ----
let buf = '';
process.stdin.setEncoding('utf8');
process.stdin.on('data', chunk => {
  buf += chunk;
  const lines = buf.split('\n');
  buf = lines.pop();
  for (const line of lines) {
    const trimmed = line.trim();
    if (trimmed) {
      try { dispatch(JSON.parse(trimmed)); } catch (_) {}
    }
  }
});
process.stdin.on('end', () => process.exit(0));

function send(obj) {
  process.stdout.write(JSON.stringify(obj) + '\n');
}

function dispatch(msg) {
  switch (msg.method) {
    case 'initialize':
      send({ jsonrpc: '2.0', id: msg.id, result: {
        protocolVersion: '2024-11-05',
        capabilities: { tools: {} },
        serverInfo: { name: 'grants-gov', version: '1.0.0' }
      }});
      break;
    case 'notifications/initialized':
      break;
    case 'tools/list':
      send({ jsonrpc: '2.0', id: msg.id, result: { tools: TOOLS } });
      break;
    case 'tools/call':
      runTool(msg);
      break;
    default:
      if (msg.id != null) {
        send({ jsonrpc: '2.0', id: msg.id, error: { code: -32601, message: 'Method not found' } });
      }
  }
}

// ---- tool definitions ----
const TOOLS = [
  {
    name: 'search_grants',
    description: 'Search Grants.gov for federal grant opportunities. Returns title, agency, deadline, synopsis, and a direct link for each result.',
    inputSchema: {
      type: 'object',
      properties: {
        keywords: {
          type: 'string',
          description: 'Search terms, e.g. "nonprofit workforce development Minnesota"'
        },
        agency: {
          type: 'string',
          description: 'Optional agency code filter, e.g. HHS, DOJ, HUD, DOE, USDA, DOL'
        },
        status: {
          type: 'string',
          enum: ['posted', 'forecasted', 'closed', 'archived'],
          description: 'Opportunity status — default: posted (currently open)'
        },
        rows: {
          type: 'integer',
          description: 'Number of results to return (1-25, default 10)'
        }
      },
      required: ['keywords']
    }
  },
  {
    name: 'get_grant_detail',
    description: 'Get full details for a specific grant opportunity by its Grants.gov opportunity ID.',
    inputSchema: {
      type: 'object',
      properties: {
        opportunity_id: {
          type: 'string',
          description: 'The numeric opportunity ID from a search_grants result'
        }
      },
      required: ['opportunity_id']
    }
  }
];

// ---- tool execution ----
async function runTool(msg) {
  const { name, arguments: args } = msg.params;
  try {
    let text;
    if (name === 'search_grants') text = await searchGrants(args);
    else if (name === 'get_grant_detail') text = await getGrantDetail(args);
    else throw new Error(`Unknown tool: ${name}`);
    send({ jsonrpc: '2.0', id: msg.id, result: { content: [{ type: 'text', text }] } });
  } catch (err) {
    send({ jsonrpc: '2.0', id: msg.id, result: {
      content: [{ type: 'text', text: `Error: ${err.message}` }],
      isError: true
    }});
  }
}

// ---- Grants.gov API calls ----
function httpsPost(url, body) {
  return new Promise((resolve, reject) => {
    const u = new URL(url);
    const data = JSON.stringify(body);
    const req = https.request({
      hostname: u.hostname,
      path: u.pathname + (u.search || ''),
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'Content-Length': Buffer.byteLength(data),
        'User-Agent': 'colosseum-mcp-grants/1.0'
      },
      timeout: 20000
    }, res => {
      let chunks = '';
      res.on('data', c => chunks += c);
      res.on('end', () => {
        if (res.statusCode >= 400) return reject(new Error(`Grants.gov API HTTP ${res.statusCode}: ${chunks.slice(0, 200)}`));
        try { resolve(JSON.parse(chunks)); }
        catch (e) { reject(new Error(`Failed to parse Grants.gov response: ${chunks.slice(0, 200)}`)); }
      });
    });
    req.on('error', reject);
    req.on('timeout', () => { req.destroy(); reject(new Error('Grants.gov API request timed out')); });
    req.write(data);
    req.end();
  });
}

function httpsGet(url) {
  return new Promise((resolve, reject) => {
    const u = new URL(url);
    const req = https.request({
      hostname: u.hostname,
      path: u.pathname + (u.search || ''),
      method: 'GET',
      headers: { 'User-Agent': 'colosseum-mcp-grants/1.0' },
      timeout: 20000
    }, res => {
      let chunks = '';
      res.on('data', c => chunks += c);
      res.on('end', () => {
        if (res.statusCode >= 400) return reject(new Error(`Grants.gov API HTTP ${res.statusCode}`));
        try { resolve(JSON.parse(chunks)); }
        catch (e) { reject(new Error('Failed to parse response')); }
      });
    });
    req.on('error', reject);
    req.on('timeout', () => { req.destroy(); reject(new Error('Request timed out')); });
    req.end();
  });
}

async function searchGrants(args) {
  const rows = Math.min(Math.max(parseInt(args.rows) || 10, 1), 25);
  const body = {
    keyword: args.keywords || '',
    oppStatuses: args.status || 'posted',
    rows,
    startRecordNum: 0,
    sortBy: 'openDate|desc'
  };
  if (args.agency) body.agencyCode = args.agency.toUpperCase();

  const data = await httpsPost('https://api.grants.gov/v1/api/search2', body);
  const hits = data.oppHits || [];
  if (hits.length === 0) return `No ${args.status || 'posted'} grants found for "${args.keywords}".`;

  const total = data.totalRecords || hits.length;
  const lines = [`Found ${total} grants matching "${args.keywords}" (showing ${hits.length}):\n`];

  for (const o of hits) {
    const closeDate = o.closeDate || o.closeDateStr || 'No deadline listed';
    const openDate  = o.openDate  || o.openDateStr  || 'N/A';
    const synopsis  = o.synopsis  ? o.synopsis.replace(/<[^>]+>/g, '').slice(0, 400) + '...' : 'No synopsis available.';
    lines.push(
      `Title: ${o.title || o.oppTitle || 'Untitled'}`,
      `Agency: ${o.agencyName || o.agencyCode || 'Unknown'}`,
      `Opportunity #: ${o.number || o.oppNumber || 'N/A'}`,
      `ID: ${o.id || o.oppId || 'N/A'}`,
      `Posted: ${openDate} | Deadline: ${closeDate}`,
      `URL: https://www.grants.gov/search-results-detail/${o.id || o.oppId}`,
      `Synopsis: ${synopsis}`,
      ''
    );
  }
  return lines.join('\n');
}

async function getGrantDetail(args) {
  const id = String(args.opportunity_id).trim();
  const data = await httpsGet(`https://api.grants.gov/v1/api/fetchOpportunity?oppId=${encodeURIComponent(id)}`);
  const o = data.opportunityDetail || data;
  if (!o || (!o.title && !o.oppTitle)) return `No grant found with ID ${id}.`;

  const lines = [
    `**${o.title || o.oppTitle}**`,
    `Opportunity Number: ${o.number || o.oppNumber || 'N/A'}`,
    `Agency: ${o.agencyName || 'N/A'} (${o.agencyCode || 'N/A'})`,
    `Status: ${o.oppStatus || 'N/A'}`,
    `Posted: ${o.openDate || 'N/A'} | Deadline: ${o.closeDate || 'N/A'}`,
    `Award ceiling: ${o.awardCeiling ? '$' + Number(o.awardCeiling).toLocaleString() : 'N/A'}`,
    `Award floor: ${o.awardFloor ? '$' + Number(o.awardFloor).toLocaleString() : 'N/A'}`,
    `Expected awards: ${o.expectedNumberOfAwards || 'N/A'}`,
    `Eligibility: ${(o.eligibilities || []).join(', ') || 'N/A'}`,
    `CFDA: ${(o.cfdaNumbers || []).join(', ') || 'N/A'}`,
    `URL: https://www.grants.gov/search-results-detail/${id}`,
    ``,
    `Description:`,
    (o.description || o.synopsis || 'No description available.').replace(/<[^>]+>/g, '').slice(0, 2000)
  ];
  return lines.join('\n');
}
