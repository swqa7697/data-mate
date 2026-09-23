package sqlpolicy

// Catalog records explicitly retain every audited field. Strict decoding requires
// each property, distinguishing omission from a present false, zero, or null.
type scalarType struct {
	OID            uint32  `json:"oid"`
	Typalign       string  `json:"typalign"`
	Typanalyze     uint32  `json:"typanalyze"`
	Typarray       uint32  `json:"typarray"`
	Typbasetype    uint32  `json:"typbasetype"`
	Typbyval       bool    `json:"typbyval"`
	Typcategory    string  `json:"typcategory"`
	Typcollation   uint32  `json:"typcollation"`
	Typdefault     *string `json:"typdefault"`
	Typdefaultbin  *string `json:"typdefaultbin"`
	Typdelim       string  `json:"typdelim"`
	Typelem        uint32  `json:"typelem"`
	Typinput       uint32  `json:"typinput"`
	Typisdefined   bool    `json:"typisdefined"`
	Typispreferred bool    `json:"typispreferred"`
	Typlen         int32   `json:"typlen"`
	Typmodin       uint32  `json:"typmodin"`
	Typmodout      uint32  `json:"typmodout"`
	Typname        string  `json:"typname"`
	Typnamespace   uint32  `json:"typnamespace"`
	Typndims       int32   `json:"typndims"`
	Typnotnull     bool    `json:"typnotnull"`
	Typoutput      uint32  `json:"typoutput"`
	Typreceive     uint32  `json:"typreceive"`
	Typrelid       uint32  `json:"typrelid"`
	Typsend        uint32  `json:"typsend"`
	Typstorage     string  `json:"typstorage"`
	Typsubscript   uint32  `json:"typsubscript"`
	Typtype        string  `json:"typtype"`
	Typtypmod      int32   `json:"typtypmod"`
}

type operator struct {
	OID          uint32 `json:"oid"`
	Oprcanhash   bool   `json:"oprcanhash"`
	Oprcanmerge  bool   `json:"oprcanmerge"`
	Oprcode      uint32 `json:"oprcode"`
	Oprcom       uint32 `json:"oprcom"`
	Oprjoin      uint32 `json:"oprjoin"`
	Oprkind      string `json:"oprkind"`
	Oprleft      uint32 `json:"oprleft"`
	Oprname      string `json:"oprname"`
	Oprnamespace uint32 `json:"oprnamespace"`
	Oprnegate    uint32 `json:"oprnegate"`
	Oprrest      uint32 `json:"oprrest"`
	Oprresult    uint32 `json:"oprresult"`
	Oprright     uint32 `json:"oprright"`
}

type aggregate struct {
	Aggcombinefn     uint32  `json:"aggcombinefn"`
	Aggdeserialfn    uint32  `json:"aggdeserialfn"`
	Aggfinalextra    bool    `json:"aggfinalextra"`
	Aggfinalfn       uint32  `json:"aggfinalfn"`
	Aggfinalmodify   string  `json:"aggfinalmodify"`
	Aggfnoid         uint32  `json:"aggfnoid"`
	Agginitval       *string `json:"agginitval"`
	Aggkind          string  `json:"aggkind"`
	Aggmfinalextra   bool    `json:"aggmfinalextra"`
	Aggmfinalfn      uint32  `json:"aggmfinalfn"`
	Aggmfinalmodify  string  `json:"aggmfinalmodify"`
	Aggminitval      *string `json:"aggminitval"`
	Aggminvtransfn   uint32  `json:"aggminvtransfn"`
	Aggmtransfn      uint32  `json:"aggmtransfn"`
	Aggmtranstype    uint32  `json:"aggmtranstype"`
	Aggnumdirectargs int32   `json:"aggnumdirectargs"`
	Aggserialfn      uint32  `json:"aggserialfn"`
	Aggsortop        uint32  `json:"aggsortop"`
	Aggtransfn       uint32  `json:"aggtransfn"`
	Aggtranstype     uint32  `json:"aggtranstype"`
}

type cast struct {
	Castcontext string `json:"castcontext"`
	Castfunc    uint32 `json:"castfunc"`
	Castmethod  string `json:"castmethod"`
	Castsource  uint32 `json:"castsource"`
	Casttarget  uint32 `json:"casttarget"`
}

type operatorClass struct {
	Opcdefault   bool   `json:"opcdefault"`
	Opcfamily    uint32 `json:"opcfamily"`
	Opcintype    uint32 `json:"opcintype"`
	Opckeytype   uint32 `json:"opckeytype"`
	Opcmethod    uint32 `json:"opcmethod"`
	Opcname      string `json:"opcname"`
	Opcnamespace uint32 `json:"opcnamespace"`
}

type familyMember struct {
	Amopfamily     uint32 `json:"amopfamily"`
	Amoplefttype   uint32 `json:"amoplefttype"`
	Amopmethod     uint32 `json:"amopmethod"`
	Amopopr        uint32 `json:"amopopr"`
	Amoppurpose    string `json:"amoppurpose"`
	Amoprighttype  uint32 `json:"amoprighttype"`
	Amopsortfamily uint32 `json:"amopsortfamily"`
	Amopstrategy   int32  `json:"amopstrategy"`
}

type familySupport struct {
	Amproc          uint32 `json:"amproc"`
	Amprocfamily    uint32 `json:"amprocfamily"`
	Amproclefttype  uint32 `json:"amproclefttype"`
	Amprocnum       int32  `json:"amprocnum"`
	Amprocrighttype uint32 `json:"amprocrighttype"`
}

type procedure struct {
	OID             uint32   `json:"oid"`
	Proallargtypes  []uint32 `json:"proallargtypes"`
	Proargdefaults  *string  `json:"proargdefaults"`
	Proargmodes     []string `json:"proargmodes"`
	Proargnames     []string `json:"proargnames"`
	Proargtypes     []uint32 `json:"proargtypes"`
	Probin          *string  `json:"probin"`
	Proconfig       []string `json:"proconfig"`
	Proisstrict     bool     `json:"proisstrict"`
	Prokind         string   `json:"prokind"`
	Prolang         uint32   `json:"prolang"`
	Proleakproof    bool     `json:"proleakproof"`
	Proname         string   `json:"proname"`
	Pronamespace    uint32   `json:"pronamespace"`
	Pronargdefaults int32    `json:"pronargdefaults"`
	Pronargs        int32    `json:"pronargs"`
	Proparallel     string   `json:"proparallel"`
	Proretset       bool     `json:"proretset"`
	Prorettype      uint32   `json:"prorettype"`
	Prosecdef       bool     `json:"prosecdef"`
	Prosqlbody      *string  `json:"prosqlbody"`
	Prosrc          string   `json:"prosrc"`
	Prosupport      uint32   `json:"prosupport"`
	Protrftypes     []uint32 `json:"protrftypes"`
	Provariadic     uint32   `json:"provariadic"`
	Provolatile     string   `json:"provolatile"`
}

type catalogDocument struct {
	Types      []scalarType    `json:"types"`
	Operators  []operator      `json:"operators"`
	Aggregates []aggregate     `json:"aggregates"`
	Casts      []cast          `json:"casts"`
	Classes    []operatorClass `json:"classes"`
	Members    []familyMember  `json:"members"`
	Supports   []familySupport `json:"supports"`
	Functions  []procedure     `json:"functions"`
}

func (r *scalarType) UnmarshalJSON(b []byte) error { return decodeRecord(b, r) }

func (r *operator) UnmarshalJSON(b []byte) error { return decodeRecord(b, r) }

func (r *aggregate) UnmarshalJSON(b []byte) error { return decodeRecord(b, r) }

func (r *cast) UnmarshalJSON(b []byte) error { return decodeRecord(b, r) }

func (r *operatorClass) UnmarshalJSON(b []byte) error { return decodeRecord(b, r) }

func (r *familyMember) UnmarshalJSON(b []byte) error { return decodeRecord(b, r) }

func (r *familySupport) UnmarshalJSON(b []byte) error { return decodeRecord(b, r) }

func (r *procedure) UnmarshalJSON(b []byte) error { return decodeRecord(b, r) }

func (r *catalogDocument) UnmarshalJSON(b []byte) error { return decodeRecord(b, r) }
