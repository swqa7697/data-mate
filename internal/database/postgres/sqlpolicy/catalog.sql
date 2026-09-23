WITH RECURSIVE types AS (
 SELECT * FROM pg_catalog.pg_type WHERE oid=ANY(ARRAY[16,17,20,21,23,25,114,700,701,1042,1043,1082,1083,1114,1184,1700,2950,3802]::oid[])
), ops AS (
 SELECT o.* FROM pg_catalog.pg_operator o WHERE o.oprnamespace=11
 AND ((o.oprleft IN (SELECT oid FROM types) AND o.oprright=o.oprleft AND o.oprname IN ('=','<>','<','<=','>','>=','+','-','*','/'))
 OR (o.oprleft=0 AND o.oprright IN (20,21,23,700,701,1700) AND o.oprname='-'))
), aggs AS (
 SELECT a.* FROM pg_catalog.pg_aggregate a JOIN pg_catalog.pg_proc p ON p.oid=a.aggfnoid
 WHERE p.pronamespace=11 AND ((p.proname IN ('sum','avg','min','max') AND p.pronargs=1 AND p.proargtypes[0] IN (SELECT oid FROM types)) OR p.proname='count')
), casts AS (
 SELECT * FROM pg_catalog.pg_cast WHERE castsource IN (20,21,23,700,701,1700) AND casttarget IN (20,21,23,700,701,1700)
), classes AS (
 SELECT o.* FROM pg_catalog.pg_opclass o WHERE o.opcmethod=403 AND o.opcnamespace=11 AND o.opcintype IN (SELECT oid FROM types)
), members AS (
 SELECT m.* FROM pg_catalog.pg_amop m WHERE m.amopfamily IN (SELECT opcfamily FROM classes)
), supports AS (
 SELECT p.* FROM pg_catalog.pg_amproc p WHERE p.amprocfamily IN (SELECT opcfamily FROM classes)
), roots AS (
 SELECT p.* FROM pg_catalog.pg_proc p WHERE p.oid IN (
 SELECT oprcode FROM ops UNION SELECT t.typinput FROM types t UNION SELECT t.typoutput FROM types t
 UNION SELECT t.typreceive FROM types t UNION SELECT t.typsend FROM types t
 UNION SELECT typmodin FROM types UNION SELECT typmodout FROM types
 UNION SELECT typanalyze FROM types UNION SELECT typsubscript FROM types
 UNION SELECT oprrest FROM ops UNION SELECT oprjoin FROM ops
 UNION SELECT castfunc FROM casts UNION SELECT amproc FROM supports
 UNION SELECT aggfnoid FROM aggs UNION SELECT aggtransfn FROM aggs UNION SELECT aggfinalfn FROM aggs
 UNION SELECT aggcombinefn FROM aggs UNION SELECT aggserialfn FROM aggs UNION SELECT aggdeserialfn FROM aggs
 UNION SELECT aggmtransfn FROM aggs UNION SELECT aggminvtransfn FROM aggs UNION SELECT aggmfinalfn FROM aggs
 UNION SELECT o.oprcode FROM pg_catalog.pg_operator o JOIN members m ON m.amopopr=o.oid)
), procs AS (
 SELECT * FROM roots
 UNION
 SELECT p.* FROM pg_catalog.pg_proc p JOIN procs parent ON p.oid=parent.prosupport
)
SELECT pg_catalog.jsonb_build_object(
 'types',(SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
  'oid',x.oid::oid::bigint,
  'typalign',x.typalign,
  'typanalyze',x.typanalyze::oid::bigint,
  'typarray',x.typarray::oid::bigint,
  'typbasetype',x.typbasetype::oid::bigint,
  'typbyval',x.typbyval,
  'typcategory',x.typcategory,
  'typcollation',x.typcollation::oid::bigint,
  'typdefault',x.typdefault,
  'typdefaultbin',x.typdefaultbin,
  'typdelim',x.typdelim,
  'typelem',x.typelem::oid::bigint,
  'typinput',x.typinput::oid::bigint,
  'typisdefined',x.typisdefined,
  'typispreferred',x.typispreferred,
  'typlen',x.typlen,
  'typmodin',x.typmodin::oid::bigint,
  'typmodout',x.typmodout::oid::bigint,
  'typname',x.typname,
  'typnamespace',x.typnamespace::oid::bigint,
  'typndims',x.typndims,
  'typnotnull',x.typnotnull,
  'typoutput',x.typoutput::oid::bigint,
  'typreceive',x.typreceive::oid::bigint,
  'typrelid',x.typrelid::oid::bigint,
  'typsend',x.typsend::oid::bigint,
  'typstorage',x.typstorage,
  'typsubscript',x.typsubscript::oid::bigint,
  'typtype',x.typtype,
  'typtypmod',x.typtypmod) ORDER BY x.oid) FROM types x),
 'operators',(SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
  'oid',x.oid::oid::bigint,
  'oprcanhash',x.oprcanhash,
  'oprcanmerge',x.oprcanmerge,
  'oprcode',x.oprcode::oid::bigint,
  'oprcom',x.oprcom::oid::bigint,
  'oprjoin',x.oprjoin::oid::bigint,
  'oprkind',x.oprkind,
  'oprleft',x.oprleft::oid::bigint,
  'oprname',x.oprname,
  'oprnamespace',x.oprnamespace::oid::bigint,
  'oprnegate',x.oprnegate::oid::bigint,
  'oprrest',x.oprrest::oid::bigint,
  'oprresult',x.oprresult::oid::bigint,
  'oprright',x.oprright::oid::bigint) ORDER BY x.oid) FROM ops x),
 'aggregates',(SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
  'aggcombinefn',x.aggcombinefn::oid::bigint,
  'aggdeserialfn',x.aggdeserialfn::oid::bigint,
  'aggfinalextra',x.aggfinalextra,
  'aggfinalfn',x.aggfinalfn::oid::bigint,
  'aggfinalmodify',x.aggfinalmodify,
  'aggfnoid',x.aggfnoid::oid::bigint,
  'agginitval',x.agginitval,
  'aggkind',x.aggkind,
  'aggmfinalextra',x.aggmfinalextra,
  'aggmfinalfn',x.aggmfinalfn::oid::bigint,
  'aggmfinalmodify',x.aggmfinalmodify,
  'aggminitval',x.aggminitval,
  'aggminvtransfn',x.aggminvtransfn::oid::bigint,
  'aggmtransfn',x.aggmtransfn::oid::bigint,
  'aggmtranstype',x.aggmtranstype::oid::bigint,
  'aggnumdirectargs',x.aggnumdirectargs,
  'aggserialfn',x.aggserialfn::oid::bigint,
  'aggsortop',x.aggsortop::oid::bigint,
  'aggtransfn',x.aggtransfn::oid::bigint,
  'aggtranstype',x.aggtranstype::oid::bigint) ORDER BY x.aggfnoid::oid) FROM aggs x),
 'casts',(SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
  'castcontext',x.castcontext,
  'castfunc',x.castfunc::oid::bigint,
  'castmethod',x.castmethod,
  'castsource',x.castsource::oid::bigint,
  'casttarget',x.casttarget::oid::bigint) ORDER BY x.castsource,x.casttarget) FROM casts x),
 'classes',(SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
  'opcdefault',x.opcdefault,
  'opcfamily',x.opcfamily::oid::bigint,
  'opcintype',x.opcintype::oid::bigint,
  'opckeytype',x.opckeytype::oid::bigint,
  'opcmethod',x.opcmethod::oid::bigint,
  'opcname',x.opcname,
  'opcnamespace',x.opcnamespace::oid::bigint) ORDER BY x.opcnamespace,x.opcmethod,x.opcname) FROM classes x),
 'members',(SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
  'amopfamily',x.amopfamily::oid::bigint,
  'amoplefttype',x.amoplefttype::oid::bigint,
  'amopmethod',x.amopmethod::oid::bigint,
  'amopopr',x.amopopr::oid::bigint,
  'amoppurpose',x.amoppurpose,
  'amoprighttype',x.amoprighttype::oid::bigint,
  'amopsortfamily',x.amopsortfamily::oid::bigint,
  'amopstrategy',x.amopstrategy) ORDER BY x.amopfamily,x.amoplefttype,x.amoprighttype,x.amopstrategy,x.amoppurpose) FROM members x),
 'supports',(SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
  'amproc',x.amproc::oid::bigint,
  'amprocfamily',x.amprocfamily::oid::bigint,
  'amproclefttype',x.amproclefttype::oid::bigint,
  'amprocnum',x.amprocnum,
  'amprocrighttype',x.amprocrighttype::oid::bigint) ORDER BY x.amprocfamily,x.amproclefttype,x.amprocrighttype,x.amprocnum) FROM supports x),
 'functions',(SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
  'oid',x.oid::oid::bigint,
  'proallargtypes',x.proallargtypes::oid[]::bigint[],
  'proargdefaults',x.proargdefaults,
  'proargmodes',x.proargmodes,
  'proargnames',x.proargnames,
  'proargtypes',x.proargtypes::oid[]::bigint[],
  'probin',x.probin,
  'proconfig',x.proconfig,
  'proisstrict',x.proisstrict,
  'prokind',x.prokind,
  'prolang',x.prolang::oid::bigint,
  'proleakproof',x.proleakproof,
  'proname',x.proname,
  'pronamespace',x.pronamespace::oid::bigint,
  'pronargdefaults',x.pronargdefaults,
  'pronargs',x.pronargs,
  'proparallel',x.proparallel,
  'proretset',x.proretset,
  'prorettype',x.prorettype::oid::bigint,
  'prosecdef',x.prosecdef,
  'prosqlbody',x.prosqlbody,
  'prosrc',x.prosrc,
  'prosupport',x.prosupport::oid::bigint,
  'protrftypes',x.protrftypes::oid[]::bigint[],
  'provariadic',x.provariadic::oid::bigint,
  'provolatile',x.provolatile) ORDER BY x.oid) FROM procs x)
)::text
