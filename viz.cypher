// ============================================================
// infinite-memory graph visualization queries (Memgraph Lab)
// Lab: http://localhost:3000  (Quick Connect -> memgraph:7687)
//
// Schema
//   (:Project  {key, name, created_at})
//   (:Session  {id, project_key, started_at, updated_at, last_extracted_line})
//   (:Memory   {hash, id, title, title_lc, content, kind, keywords,
//               project_key, created_at, last_seen_at, seen_count,
//               superseded, source_session})
//   (:Entity   {key, name, name_lc, tokens, etype, project_key, created_at})
//
//   (:Session)-[:IN_PROJECT]->(:Project)
//   (:Memory)-[:IN_PROJECT]->(:Project)
//   (:Memory)-[:IN_SESSION]->(:Session)
//   (:Memory)-[:SUPERSEDES]->(:Memory)
//   (:Memory)-[:MENTIONS]->(:Entity)
//   (:Entity)-[:RELATED {weight, verb, last_seen_at}]->(:Entity)
// ============================================================


// --- 1. WHOLE GRAPH (safe: ~285 nodes today) -----------------
MATCH p = (n)-[r]->(m)
RETURN p;


// --- 2. ENTITY KNOWLEDGE GRAPH (topic map, verbs on edges) ---
MATCH p = (a:Entity)-[r:RELATED]->(b:Entity)
WHERE r.weight >= 2
RETURN p
ORDER BY r.weight DESC
LIMIT 300;


// --- 3. ONE ENTITY NEIGHBORHOOD, 2 HOPS ----------------------
// :param name => "memgraph"
MATCH p = (e:Entity)-[:RELATED *1..2]-(nb:Entity)
WHERE e.name_lc CONTAINS toLower($name)
RETURN p
LIMIT 200;


// --- 4. ENTITY + THE MEMORIES THAT MENTION IT ----------------
MATCH (e:Entity) WHERE e.name_lc CONTAINS toLower($name)
MATCH p = (m:Memory)-[:MENTIONS]->(e)
RETURN p;


// --- 5. PROJECT BACKBONE (project -> sessions -> memories) ---
// :param pk => "/Users/ammardwianwari/play/infinite-memory"
MATCH p = (m:Memory)-[:IN_SESSION]->(s:Session)-[:IN_PROJECT]->(pr:Project {key: $pk})
RETURN p;


// --- 6. ONE PROJECT'S MEMORY->ENTITY SUBGRAPH ----------------
MATCH (m:Memory {project_key: $pk})
WHERE NOT coalesce(m.superseded, false)
MATCH p = (m)-[:MENTIONS]->(:Entity)
RETURN p
LIMIT 400;


// --- 7. SUPERSESSION CHAINS (what got overwritten) -----------
MATCH p = (new:Memory)-[:SUPERSEDES *]->(old:Memory)
RETURN p;


// --- 8. RULE MEMORIES (standing constitution) ----------------
MATCH p = (m:Memory {kind: "rule"})-[:MENTIONS]->(:Entity)
RETURN p;


// --- 9. HUB ENTITIES (table, for picking a seed for #3) ------
MATCH (e:Entity)
OPTIONAL MATCH (e)<-[:MENTIONS]-(m:Memory)
OPTIONAL MATCH (e)-[r:RELATED]-()
RETURN e.name AS entity, e.etype AS type, e.project_key AS project,
       count(DISTINCT m) AS mentions, count(DISTINCT r) AS degree
ORDER BY mentions DESC, degree DESC
LIMIT 30;


// --- 10. CROSS-PROJECT ENTITIES (same topic, many repos) -----
MATCH (e:Entity)
WITH e.name_lc AS n, collect(DISTINCT e.project_key) AS projects
WHERE size(projects) > 1
RETURN n AS entity, projects, size(projects) AS cnt
ORDER BY cnt DESC;


// --- 11. HEALTH COUNTS --------------------------------------
MATCH (n) RETURN labels(n)[0] AS label, count(*) AS c ORDER BY c DESC;
MATCH ()-[r]->() RETURN type(r) AS rel, count(*) AS c ORDER BY c DESC;
