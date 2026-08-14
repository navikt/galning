package oauth

// Team is a GitHub team in the organisation that a user belongs to. It is the
// unit a user picks to scope a Query; the scope resolves to the team's repos.
type Team struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}
