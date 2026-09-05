DELETE FROM provider_cache
WHERE instr(lower(CAST(results_json AS TEXT)), 'api_key=') > 0;

DELETE FROM candidates
WHERE instr(lower(result_id), 'api_key=') > 0
   OR instr(lower(CAST(metadata_json AS TEXT)), 'api_key=') > 0;

DELETE FROM pack_cache
WHERE instr(lower(result_id), 'api_key=') > 0
   OR instr(lower(CAST(candidate_json AS TEXT)), 'api_key=') > 0;

DELETE FROM installations
WHERE instr(lower(candidate_id), 'api_key=') > 0
   OR instr(lower(CAST(score_json AS TEXT)), 'api_key=') > 0
   OR instr(lower(CAST(sync_result_json AS TEXT)), 'api_key=') > 0;

DELETE FROM candidate_rejections
WHERE instr(lower(result_id), 'api_key=') > 0;
