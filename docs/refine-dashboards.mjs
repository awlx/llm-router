export const sizeBounds = [128, 512, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 33554432];

export function boundPattern(value) {
  if (value === Infinity) return '\\+Inf';
  const [mantissa, exponent] = value.toExponential().split('e+');
  return `${value}(\\.0)?|${mantissa.replaceAll('.', '\\.')}e\\+0?${exponent}`;
}

function bytesLabel(value) {
  if (value >= 1048576) return `${value / 1048576} MiB`;
  if (value >= 1024) return `${value / 1024} KiB`;
  return `${value} B`;
}

export function sizeBucketTargets(metric, selector) {
  const bounds = [...sizeBounds, Infinity];
  const cumulative = value => `sum(increase(${metric}{${selector},le=~${JSON.stringify(boundPattern(value))}}[$__range]))`;
  return bounds.map((bound, index) => ({
    refId: `B${index}`,
    expr: index === 0 ? cumulative(bound) : `clamp_min(${cumulative(bound)} - ${cumulative(bounds[index - 1])}, 0)`,
    legendFormat: index === 0 ? `0 - ${bytesLabel(bound)}` : bound === Infinity ? `> ${bytesLabel(bounds[index - 1])}` : `${bytesLabel(bounds[index - 1])} - ${bytesLabel(bound)}`,
    instant: true,
    range: false,
  }));
}

function addThroughputGauges(dashboard) {
  if (dashboard.uid !== 'llm-platform-overview') return dashboard;
  const flatten = panels => panels.flatMap(panel => [panel, ...flatten(panel.panels || [])]);
  const all = flatten(dashboard.panels);
  if (all.some(panel => panel.title === 'Current decode per model (1m rate)')) return dashboard;
  const rowIndex = dashboard.panels.findIndex(panel => panel.type === 'row' && panel.title === 'Workload');
  if (rowIndex < 0) throw new Error('Missing overview workload row');
  const start = dashboard.panels[rowIndex].gridPos.y + 1;
  let nextId = Math.max(...all.map(panel => panel.id)) + 1;
  const gauges = ['Decode', 'Prefill'].map((phase, index) => {
    const source = all.find(panel => panel.title === `${phase} token throughput`);
    if (!source) throw new Error(`Missing ${phase} throughput chart`);
    const panel = structuredClone(source);
    panel.id = nextId++;
    panel.title = `Current ${phase.toLowerCase()} per model (1m rate)`;
    panel.type = 'gauge';
    panel.description = 'Latest one-minute token rate, including idle time, summed per served model. This is not active-generation speed or hardware utilization. Each arc scales to that model\'s observed peak over the selected time range. Missing data is not zero-filled.';
    panel.targets = [{ refId: 'A', expr: `sum by (model) (${source.targets[0].expr.replace('[$__rate_interval]', '[1m]')})`, legendFormat: '{{model}}', instant: false, range: true }];
    panel.options = { reduceOptions: { calcs: ['lastNotNull'], fields: '', values: false }, orientation: 'auto', showThresholdLabels: false, showThresholdMarkers: false, sizing: 'auto', minVizWidth: 90, minVizHeight: 90 };
    panel.fieldConfig.defaults = { unit: 'suffix:tokens/s', min: 0, fieldMinMax: true, decimals: 1, noValue: 'No data' };
    panel.gridPos = { x: index * 12, y: start, w: 12, h: 5 };
    return panel;
  });
  for (const panel of all) if (panel.gridPos.y >= start) panel.gridPos.y += 5;
  dashboard.panels.splice(rowIndex + 1, 0, ...gauges);
  return dashboard;
}

function organizeDashboard(dashboard) {
  if (!['llm-platform-overview', 'llm-router-telemetry'].includes(dashboard.uid)) return dashboard;
  if (dashboard.description?.includes('[grouping-v3]')) return dashboard;
  const flatten = panels => panels.flatMap(panel => [panel, ...flatten(panel.panels || [])]);
  if (dashboard.uid === 'llm-platform-overview') {
    const original = flatten(dashboard.panels);
    const used = new Set();
    let nextId = Math.max(...original.map(panel => panel.id)) + 1;
    let cursor = 0;
    const panels = [];
    let group = panels;
    const take = title => {
      const panel = original.find(candidate => candidate.type !== 'row' && candidate.title === title);
      if (!panel) throw new Error(`Missing overview panel: ${title}`);
      used.add(panel.id);
      return panel;
    };
    const section = (title, collapsed = false) => {
      const row = { id: nextId++, title, type: 'row', collapsed, gridPos: { x: 0, y: cursor++, w: 24, h: 1 }, panels: [] };
      panels.push(row);
      group = collapsed ? row.panels : panels;
    };
    const place = (items, height = 8) => {
      items.forEach((panel, index) => {
        panel.gridPos = { x: index * 24 / items.length, y: cursor, w: 24 / items.length, h: height };
        group.push(panel);
      });
      cursor += height;
    };
    section('Health & Reliability');
    place(['Prometheus scrape health', 'Fleet node transport', 'Engine health'].map(take), 3);
    place(['Router errors / rejections / cancellations', 'Host sample age'].map(take), 5);
    section('Throughput');
    place(['Current decode per model (1m rate)', 'Current prefill per model (1m rate)'].map(take), 5);
    place(['Decode token throughput', 'Prefill token throughput'].map(take), 7);
    place([take('Request rates: router / engines')], 6);
    section('Concurrency & Pressure');
    const concurrency = take('Open streams / running / waiting');
    const separateConcurrency = ['Router open streams', 'Engine running requests', 'Engine waiting requests'].map((title, index) => {
      const panel = structuredClone(concurrency);
      panel.id = index === 0 ? concurrency.id : nextId++;
      panel.title = title;
      panel.targets = [{ ...panel.targets[index], refId: 'A', legendFormat: '{{model}}' }];
      return panel;
    });
    place(separateConcurrency, 6);
    place(['Engine preemptions per second', 'KV cache utilization'].map(take), 6);
    section('Latency');
    place(['Time to first token (mean)', 'Inter-token latency (mean)', 'Engine end-to-end latency (mean)'].map(take), 7);
    place(['Full-request observations (selected range)', 'Header observations (selected range)'].map(take), 4);
    place(['Full request latency distribution', 'Response-header latency distribution'].map(take));
    section('Payloads', true);
    place(['Request payload size distribution', 'Response payload size distribution'].map(take));
    place(['Request size buckets (selected range)', 'Response size buckets (selected range)'].map(take), 10);
    place([take('Router payload traffic')]);
    section('Efficiency', true);
    place(['Prefix-cache hit ratio', 'Speculative acceptance'].map(take));
    section('Infrastructure', true);
    place(['CPU utilization', 'GPU utilization'].map(take));
    place(['Host memory', 'GPU temperature'].map(take));
    place(['GPU power', 'Host network throughput'].map(take));
    place(['Router endpoint discovery', 'Model conflicts', 'Router discovery age'].map(take), 4);
    const extra = original.filter(panel => panel.type !== 'row' && !used.has(panel.id));
    if (extra.length) {
      section('Additional panels', true);
      for (const panel of extra) place([panel]);
    }
    dashboard.panels = panels;
  } else if (dashboard.uid === 'llm-router-telemetry') {
    for (const row of dashboard.panels.filter(panel => panel.type === 'row')) {
      if (row.title === 'Router latency' || row.title === 'Payload sizes') {
        row.title = row.title === 'Router latency' ? 'Latency - $model' : 'Payloads - $model';
        row.repeat = 'model';
      }
    }
  }
  dashboard.description = `${dashboard.description || ''} [grouping-v3] Overview groups follow operational questions; router detail distributions repeat per selected model.`;
  return dashboard;
}

function guardEmptyHeatmaps(dashboard) {
  if (dashboard.uid !== 'llm-router-telemetry') return dashboard;
  const flatten = panels => panels.flatMap(panel => [panel, ...flatten(panel.panels || [])]);
  for (const panel of flatten(dashboard.panels).filter(panel => panel.type === 'heatmap')) {
    for (const target of panel.targets || []) {
      if (target.expr.includes(' @ end()')) continue;
      const metric = target.expr.match(/(llm_router_\w+)_bucket(\{[^}]+\})/);
      if (!metric) throw new Error(`Unrecognized histogram query: ${panel.title}`);
      target.expr = `(${target.expr}) and on () (sum(increase(${metric[1]}_count${metric[2]}[$__range] @ end())) > 0)`;
    }
    panel.fieldConfig.defaults.noValue = 'No observations';
  }
  return dashboard;
}

function nameUnnamedSeries(dashboard) {
  const flatten = panels => panels.flatMap(panel => [panel, ...flatten(panel.panels || [])]);
  for (const panel of flatten(dashboard.panels)) {
    for (const target of panel.targets || []) {
      if (!target.expr || target.legendFormat?.trim()) continue;
      target.legendFormat = panel.targets.length === 1 ? panel.title : `${panel.title} ${target.refId}`;
      if (panel.type === 'stat' && panel.targets.length === 1) {
        panel.fieldConfig = { ...panel.fieldConfig, defaults: { ...panel.fieldConfig?.defaults, displayName: panel.title } };
        panel.options = { ...panel.options, textMode: 'value' };
      }
    }
  }
  return signedNetworkTraffic(dashboard);
}

export function signedNetworkTraffic(dashboard) {
  const flatten = panels => panels.flatMap(panel => [panel, ...flatten(panel.panels || [])]);
  for (const panel of flatten(dashboard.panels)) {
    if (panel.type !== 'timeseries') continue;
    const receive = panel.targets?.filter(target => /^-?vllm_dashboard_node_network_receive_bytes_per_second\{[^}]*\}$/.test(target.expr || '')) || [];
    if (!receive.length) continue;
    for (const target of receive) target.expr = `-${target.expr.replace(/^-/, '')}`;
    delete panel.fieldConfig.defaults.min;
  }
  return dashboard;
}

export function refineDashboard(input) {
  const dashboard = structuredClone(input);
  if (dashboard.description?.includes('[readability-v2]')) return nameUnnamedSeries(guardEmptyHeatmaps(organizeDashboard(addThroughputGauges(dashboard))));
  const overview = dashboard.uid === 'llm-platform-overview';
  const router = dashboard.uid === 'llm-router-telemetry';
  const fleet = dashboard.uid === 'vllm-fleet-metrics';
  if (!overview && !router && !fleet) throw new Error(`Unsupported dashboard: ${dashboard.uid}`);
  const flatten = panels => panels.flatMap(panel => [panel, ...flatten(panel.panels || [])]);
  const original = flatten(dashboard.panels);
  const used = new Set();
  let nextId = Math.max(...original.map(panel => panel.id)) + 1;
  const datasource = original.find(panel => panel.datasource)?.datasource;
  const panels = [];
  let cursor = 0;
  let group = panels;
  const take = id => {
    const panel = original.find(candidate => candidate.id === id && candidate.type !== 'row');
    if (!panel) throw new Error(`Missing panel ${id} in ${dashboard.uid}`);
    used.add(id);
    return panel;
  };
  const fresh = (title, type, targets, unit = 'short') => ({
    id: nextId++, title, type, datasource: structuredClone(datasource), targets,
    fieldConfig: { defaults: { unit, min: 0 }, overrides: [] },
  });
  const target = (expr, legendFormat = '{{model}}', instant = false) => ({ refId: 'A', expr, legendFormat, instant, range: !instant });
  const section = (title, collapsed = false) => {
    const row = { id: nextId++, title, type: 'row', collapsed, gridPos: { x: 0, y: cursor++, w: 24, h: 1 }, panels: [] };
    panels.push(row);
    group = collapsed ? row.panels : panels;
  };
  const place = (items, height = 8) => {
    const width = 24 / items.length;
    items.forEach((panel, index) => {
      panel.gridPos = { x: index * width, y: cursor, w: width, h: height };
      group.push(panel);
    });
    cursor += height;
  };
  const compact = panel => {
    panel.options = { ...panel.options, colorMode: 'value', graphMode: 'none', textMode: 'value_and_name', text: { titleSize: 12, valueSize: 22 }, reduceOptions: { calcs: ['lastNotNull'], values: false } };
    panel.fieldConfig.defaults.noValue = 'No data';
    return panel;
  };
  const split = (panel, index, title, unit) => {
    const result = structuredClone(panel);
    result.id = nextId++;
    result.title = title;
    result.targets = [{ ...result.targets[index], refId: 'A' }];
    result.fieldConfig = { defaults: { unit, min: 0 }, overrides: [] };
    return result;
  };
  const routerSelector = overview
    ? 'job="$router_job",instance="$router_instance",model=~"$model",endpoint!="",route=~"$route"'
    : 'job="$job",instance="$instance",endpoint=~"$endpoint",model=~"$model",route=~"$route"';
  const engineSelector = overview
    ? 'job="$fleet_job",instance="$fleet_instance",model=~"$model"'
    : 'job="$job",instance="$instance",node=~"$node",model=~"$model"';
  const engineLatency = (title, metric, unit, multiplier = '') => {
    const panel = fresh(title, 'timeseries', [target(`${metric}{${engineSelector}}${multiplier}`)], unit);
    panel.description = 'Engine poll-interval mean across all clients, not a percentile. Zero may mean no observations during the poll. Model selector retains exact served IDs.';
    return panel;
  };
  const heatmap = (title, metric, unit) => {
    const panel = fresh(title, 'heatmap', [{ ...target(`sum by (le) (rate(${metric}_bucket{${routerSelector}}[$__rate_interval]))`, '{{le}}'), format: 'heatmap' }], 'reqps');
    panel.options = { calculate: false, cellGap: 1, color: { mode: 'scheme', scheme: 'Spectral', steps: 64 }, filterValues: { le: 0 }, legend: { show: true }, tooltip: { show: true, yHistogram: true }, yAxis: { unit, axisPlacement: 'left', reverse: false } };
    panel.description = 'Non-cumulative histogram buckets across the selected models. Color shows observations/s. Errors and partial responses are included; empty cells have no observations.';
    return panel;
  };
  const observationCount = (title, metric, range = '$__range', instant = true) => {
    const panel = fresh(title, instant ? 'stat' : 'timeseries', [target(`sum by (model) (increase(${metric}_count{${routerSelector}}[${range}]))`, '{{model}}', instant)]);
    panel.description = 'Estimated observation count from counter increase, including errors and partial responses. Prometheus extrapolates between scrapes; missing data is not zero-filled.';
    if (instant) compact(panel);
    return panel;
  };
  const quantiles = (title, metric) => {
    const panel = fresh(title, 'timeseries', [
      { quantile: 0.50, minimum: 2, refId: 'A' },
      { quantile: 0.95, minimum: 20, refId: 'B' },
      { quantile: 0.99, minimum: 100, refId: 'C' },
    ].map(({ quantile, minimum, refId }) => ({
      refId,
      expr: `histogram_quantile(${quantile}, sum by (le,model) (rate(${metric}_bucket{${routerSelector}}[$__rate_interval]))) and on (model) (sum by (model) (increase(${metric}_count{${routerSelector}}[$__rate_interval])) >= ${minimum})`,
      legendFormat: `{{model}} p${Math.round(quantile * 100)}`,
    })), 's');
    panel.description = 'Histogram estimates over $__rate_interval. p50/p95/p99 are hidden below 2/20/100 observations respectively. These are minimum sample floors, not confidence guarantees; the adjacent chart uses the identical window.';
    return panel;
  };
  const bucketBars = (title, metric) => {
    const panel = fresh(title, 'bargauge', sizeBucketTargets(metric, routerSelector));
    panel.description = 'Estimated observations during the selected dashboard time range. Each bar is a disjoint payload-byte interval (exclusive lower, inclusive upper bound), including the overflow bucket. Sizes include JSON/SSE framing, not tokens. Counts may be fractional because increase() extrapolates between scrapes.';
    panel.options = { orientation: 'horizontal', displayMode: 'basic', valueMode: 'color', showUnfilled: true, reduceOptions: { calcs: ['lastNotNull'], values: false }, sizing: 'auto', minVizHeight: 16, maxVizHeight: 30 };
    panel.fieldConfig.defaults.color = { mode: 'fixed', fixedColor: '#56AEB8' };
    return panel;
  };
  const latencySections = () => {
    const full = heatmap('Full request latency distribution', 'llm_router_request_duration_seconds', 's');
    full.description += ' Includes request-body read and the full response stream; not per-token latency.';
    const headers = heatmap('Response-header latency distribution', 'llm_router_response_headers_duration_seconds', 's');
    headers.description += ' Time to final HTTP response headers, NOT time to first generated token.';
    place([observationCount('Full-request observations (selected range)', 'llm_router_request_duration_seconds'), observationCount('Header observations (selected range)', 'llm_router_response_headers_duration_seconds')], 4);
    place([full, headers]);
  };
  const sizeSections = (requestId, responseId) => {
    place([take(requestId), take(responseId)]);
    place([bucketBars('Request size buckets (selected range)', 'llm_router_request_size_bytes_bucket'), bucketBars('Response size buckets (selected range)', 'llm_router_response_size_bytes_bucket')], 10);
  };
  const separateResources = (cpuGpu, prefixSpec) => ({
    cpu: split(cpuGpu, 0, 'CPU utilization', 'percentunit'),
    gpu: split(cpuGpu, 1, 'GPU utilization', 'percentunit'),
    prefix: split(prefixSpec, 0, 'Prefix-cache hit ratio', 'percentunit'),
    speculative: split(prefixSpec, 1, 'Speculative acceptance', 'percentunit'),
  });

  if (overview) {
    take(11);
    take(12);
    const resources = separateResources(take(18), take(16));
    place([compact(take(1)), compact(take(2)), compact(take(3))], 3);
    section('Workload');
    place([take(8), take(original.find(panel => panel.title === 'Prefill token throughput').id)], 7);
    place([take(10), engineLatency('Time to first token (mean)', 'vllm_dashboard_model_ttft_average_seconds', 's')], 7);
    place([take(9), take(17)], 7);
    section('Engine latency');
    place([engineLatency('Inter-token latency (mean)', 'vllm_dashboard_model_itl_average_seconds', 'ms', ' * 1000'), engineLatency('Engine end-to-end latency (mean)', 'vllm_dashboard_model_e2e_average_seconds', 's')], 7);
    section('Router latency');
    latencySections();
    section('Payload sizes', true);
    sizeSections(13, 14);
    section('Cache efficiency', true);
    place([take(15), resources.prefix, resources.speculative]);
    section('Discovery and freshness', true);
    place([compact(take(4)), compact(take(5)), compact(take(6)), compact(take(7))], 4);
    section('Host resources', true);
    place([resources.cpu, resources.gpu]);
    place([take(19), take(20)]);
    place([take(21), take(22)]);
    section('Traffic and preemptions', true);
    place([take(23), take(24)]);
    dashboard.links = [
      { title: 'vLLM Detail', type: 'link', url: '/d/vllm-fleet-metrics?${model:queryparam}&${node:queryparam}&var-job=${fleet_job:percentencode}&var-instance=${fleet_instance:percentencode}', keepTime: true, includeVars: false },
      { title: 'Router Detail', type: 'link', url: '/d/llm-router-telemetry?${model:queryparam}&${route:queryparam}&var-job=${router_job:percentencode}&var-instance=${router_instance:percentencode}', keepTime: true, includeVars: false },
    ];
    for (const variable of dashboard.templating.list) {
      if (['fleet_job', 'fleet_instance', 'router_job', 'router_instance'].includes(variable.name)) variable.hide = 2;
    }
  } else if (router) {
    take(7);
    take(8);
    place([compact(take(1)), compact(take(2)), compact(take(3)), compact(take(4))], 3);
    section('Workload');
    place([take(5), take(6)]);
    section('Router latency');
    latencySections();
    section('Payload sizes');
    sizeSections(9, 10);
    section('Traffic and errors');
    place([take(11), take(12)]);
    section('Latency percentiles and sample counts', true);
    place([quantiles('Full request latency percentiles', 'llm_router_request_duration_seconds'), observationCount('Full-request samples per quantile window', 'llm_router_request_duration_seconds', '$__rate_interval', false)]);
    place([quantiles('Response-header latency percentiles', 'llm_router_response_headers_duration_seconds'), observationCount('Header samples per quantile window', 'llm_router_response_headers_duration_seconds', '$__rate_interval', false)]);
    section('Discovery details', true);
    place([take(13), take(14)]);
  } else {
    take(6);
    const resources = separateResources(take(10), take(8));
    place([compact(take(1)), compact(take(2)), compact(take(3))], 3);
    section('Workload');
    place([take(4), take(original.find(panel => panel.title === 'Prefill token throughput').id)], 7);
    place([take(5), engineLatency('Time to first token (mean)', 'vllm_dashboard_model_ttft_average_seconds', 's')], 7);
    section('Engine latency');
    place([engineLatency('Inter-token latency (mean)', 'vllm_dashboard_model_itl_average_seconds', 'ms', ' * 1000'), engineLatency('Engine end-to-end latency (mean)', 'vllm_dashboard_model_e2e_average_seconds', 's')]);
    section('Cache efficiency');
    place([take(7), resources.prefix, resources.speculative]);
    section('Request completion');
    place([take(9)]);
    section('Host resources', true);
    place([resources.cpu, resources.gpu]);
    place([take(11), take(12)]);
    place([take(13), take(14)]);
  }

  const extra = original.filter(panel => panel.type !== 'row' && !used.has(panel.id));
  if (extra.length) {
    section('Additional panels', true);
    for (const panel of extra) place([panel]);
  }
  for (const panel of flatten(panels)) {
    if (!['timeseries', 'stat'].includes(panel.type)) continue;
    const hasModel = panel.targets?.some(item => /\{\{model\}\}/.test(item.legendFormat || ''));
    if (hasModel) {
      for (const item of panel.targets) {
        if (item.legendFormat?.includes('{{model}}')) item.legendFormat = item.legendFormat.replace('{{node}} / ', '').replace('{{endpoint}} / ', '');
      }
    }
    if (panel.type === 'timeseries') {
      panel.options = { ...panel.options, legend: { displayMode: 'list', placement: 'bottom', showLegend: true }, tooltip: { mode: 'multi', sort: 'desc' } };
      panel.fieldConfig.defaults.custom = { ...panel.fieldConfig.defaults.custom, lineWidth: 1, fillOpacity: 8, spanNulls: false };
    }
  }
  dashboard.panels = panels;
  dashboard.description = `${dashboard.description || ''} [readability-v2] Legends and model selectors retain exact served IDs. Latency heatmaps use router histograms; engine charts show poll means.`;
  return nameUnnamedSeries(guardEmptyHeatmaps(organizeDashboard(addThroughputGauges(dashboard))));
}