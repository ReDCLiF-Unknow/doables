// Command doables is a CLI client for the Doables server's HTTP API.
package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/go-resty/resty/v2"
	"github.com/spf13/cobra"

	"doables/internal/store"
)

type apiError struct {
	Error string `json:"error"`
}

func main() {
	var server, token string
	client := resty.New()

	root := &cobra.Command{
		Use:           "doables",
		Short:         "Manage your Doables from the terminal",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(*cobra.Command, []string) {
			client.SetBaseURL(server).SetError(&apiError{})
			if token != "" {
				client.SetAuthToken(token)
			}
		},
	}
	root.PersistentFlags().StringVarP(&server, "server", "s", envOr("DOABLES_SERVER", "http://localhost:8080"), "server base URL (env DOABLES_SERVER)")
	root.PersistentFlags().StringVarP(&token, "token", "t", os.Getenv("DOABLES_TOKEN"), "your sign-in token (env DOABLES_TOKEN); without one you only see public lists")

	// check turns a resty response into a Go error.
	check := func(resp *resty.Response, err error) error {
		if err != nil {
			return err
		}
		if resp.IsError() {
			if e, ok := resp.Error().(*apiError); ok && e.Error != "" {
				return fmt.Errorf("%s: %s", resp.Status(), e.Error)
			}
			return fmt.Errorf("%s", resp.Status())
		}
		return nil
	}

	// lists
	lists := &cobra.Command{Use: "lists", Short: "Show all lists", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var ls []store.List
			if err := check(client.R().SetResult(&ls).Get("/api/lists")); err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tOPEN\tTOTAL\tPEOPLE")
			for _, l := range ls {
				people := fmt.Sprint(l.Members)
				if l.Public() {
					people = "public"
				}
				fmt.Fprintf(w, "%d\t%s\t%d\t%d\t%s\n", l.ID, l.Name, l.Open, l.Total, people)
			}
			return w.Flush()
		}}

	newList := &cobra.Command{Use: "new-list NAME", Short: "Create a list", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var l store.List
			if err := check(client.R().SetBody(map[string]string{"name": args[0]}).SetResult(&l).Post("/api/lists")); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created list %d: %s\n", l.ID, l.Name)
			return nil
		}}

	rmList := &cobra.Command{Use: "rm-list LIST_ID", Short: "Delete a list and its tasks", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			if err := check(client.R().Delete("/api/lists/" + id)); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted list %s. It can be brought back for a day: doables restore-list %s\n", id, id)
			return nil
		}}

	restoreList := &cobra.Command{Use: "restore-list LIST_ID", Short: "Undo deleting a list, within a day", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			var l store.List
			if err := check(client.R().SetResult(&l).Post("/api/lists/" + id + "/restore")); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Restored list %d: %s\n", l.ID, l.Name)
			return nil
		}}

	// tasks
	tasks := &cobra.Command{Use: "tasks LIST_ID", Short: "Show the tasks in a list", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			var ts []store.Task
			if err := check(client.R().SetResult(&ts).Get("/api/lists/" + id + "/tasks")); err != nil {
				return err
			}
			return printTasks(cmd, ts, false)
		}}

	mine := &cobra.Command{Use: "mine", Short: "Show the open tasks assigned to you", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var ts []store.Task
			if err := check(client.R().SetResult(&ts).Get("/api/mine")); err != nil {
				return err
			}
			if len(ts) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Nothing is assigned to you.")
				return nil
			}
			return printTasks(cmd, ts, true)
		}}

	var desc, due string
	add := &cobra.Command{Use: "add LIST_ID TITLE", Short: "Add a task to a list", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			var t store.Task
			body := map[string]string{"title": args[1], "description": desc, "due_date": due}
			if err := check(client.R().SetBody(body).SetResult(&t).Post("/api/lists/" + id + "/tasks")); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Added task %d: %s\n", t.ID, t.Title)
			return nil
		}}
	add.Flags().StringVarP(&desc, "description", "d", "", "task description")
	add.Flags().StringVar(&due, "due", "", "due date, YYYY-MM-DD")

	// edit changes only the fields whose flags were given; --due "" clears the date.
	edit := &cobra.Command{Use: "edit TASK_ID", Short: "Change a task's title, description or due date", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			body := map[string]string{}
			for flag, key := range map[string]string{"title": "title", "description": "description", "due": "due_date"} {
				if cmd.Flags().Changed(flag) {
					v, _ := cmd.Flags().GetString(flag)
					body[key] = v
				}
			}
			if len(body) == 0 {
				return fmt.Errorf("nothing to change: pass --title, --description and/or --due")
			}
			var t store.Task
			if err := check(client.R().SetBody(body).SetResult(&t).Patch("/api/tasks/" + id)); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated task %d: %s\n", t.ID, t.Title)
			return nil
		}}
	edit.Flags().String("title", "", "new title")
	edit.Flags().String("description", "", "new description")
	edit.Flags().String("due", "", `new due date, YYYY-MM-DD ("" clears it)`)

	// assign takes a member's id (see `doables members`), "me", or "none".
	assign := &cobra.Command{Use: "assign TASK_ID WHO", Short: `Give a task to someone ("me", a member id, or "none")`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			var who int64
			switch strings.ToLower(args[1]) {
			case "none", "nobody", "anyone", "-":
				who = 0
			case "me":
				var u store.User
				if err := check(client.R().SetResult(&u).Get("/api/me")); err != nil {
					return err
				}
				who = u.ID
			default:
				if who, err = strconv.ParseInt(args[1], 10, 64); err != nil || who <= 0 {
					return fmt.Errorf(`invalid person %q: use "me", "none", or a member id from "doables members LIST_ID"`, args[1])
				}
			}
			var t store.Task
			if err := check(client.R().SetBody(map[string]int64{"assignee_id": who}).SetResult(&t).Patch("/api/tasks/" + id)); err != nil {
				return err
			}
			if t.Assignee == "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Task %d (%s) is now anyone's\n", t.ID, t.Title)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Task %d (%s) is now %s's\n", t.ID, t.Title, t.Assignee)
			}
			return nil
		}}

	renameList := &cobra.Command{Use: "rename-list LIST_ID NAME", Short: "Rename a list", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			var l store.List
			if err := check(client.R().SetBody(map[string]string{"name": args[1]}).SetResult(&l).Patch("/api/lists/" + id)); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Renamed list %d to %s\n", l.ID, l.Name)
			return nil
		}}

	setDone := func(use, short string, done bool) *cobra.Command {
		return &cobra.Command{Use: use + " TASK_ID", Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				id, err := parseID(args[0])
				if err != nil {
					return err
				}
				var t store.Task
				if err := check(client.R().SetBody(map[string]bool{"done": done}).SetResult(&t).Patch("/api/tasks/" + id)); err != nil {
					return err
				}
				state := "done"
				if !t.Done {
					state = "not done"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Task %d (%s) marked %s\n", t.ID, t.Title, state)
				return nil
			}}
	}

	rm := &cobra.Command{Use: "rm TASK_ID", Short: "Delete a task", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			if err := check(client.R().Delete("/api/tasks/" + id)); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted task %s\n", id)
			return nil
		}}

	// people and sharing
	register := &cobra.Command{Use: "register NAME", Short: "Create your identity and print its token", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var u struct {
				store.User
				Token string `json:"token"`
			}
			if err := check(client.R().SetBody(map[string]string{"name": args[0]}).SetResult(&u).Post("/api/users")); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Registered %s. Your token (keep it secret):\n\n  %s\n\n", u.Name, u.Token)
			fmt.Fprintf(out, "Use it with:  --token <token>   or   set DOABLES_TOKEN=<token>\n")
			return nil
		}}

	whoami := &cobra.Command{Use: "whoami", Short: "Show who you are signed in as", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var u store.User
			if err := check(client.R().SetResult(&u).Get("/api/me")); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s (id %d)\n", u.Name, u.ID)
			return nil
		}}

	invite := &cobra.Command{Use: "invite LIST_ID", Short: "Print the invite link for a list", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := findList(client, check, args[0])
			if err != nil {
				return err
			}
			if l.InviteCode == "" {
				return fmt.Errorf("list %d is public; claim it in the web UI to share it with specific people", l.ID)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s/join/%s\n", server, l.InviteCode)
			return nil
		}}

	join := &cobra.Command{Use: "join CODE_OR_LINK", Short: "Join a shared list using its invite code or link", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			code := args[0]
			if i := strings.LastIndex(code, "/join/"); i >= 0 {
				code = code[i+len("/join/"):]
			}
			var l store.List
			if err := check(client.R().SetResult(&l).Post("/api/join/" + url.PathEscape(code))); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Joined list %d: %s\n", l.ID, l.Name)
			return nil
		}}

	members := &cobra.Command{Use: "members LIST_ID", Short: "Show who is on a list", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			var ms []store.Member
			if err := check(client.R().SetResult(&ms).Get("/api/lists/" + id + "/members")); err != nil {
				return err
			}
			if len(ms) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "This list is public and has no members.")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tROLE")
			for _, m := range ms {
				role := "member"
				if m.Owner {
					role = "owner"
				}
				fmt.Fprintf(w, "%d\t%s\t%s\n", m.ID, m.Name, role)
			}
			return w.Flush()
		}}

	root.AddCommand(register, whoami, invite, join, members)
	root.AddCommand(edit, renameList, assign, mine)
	root.AddCommand(restoreList)
	root.AddCommand(lists, newList, rmList, tasks, add,
		setDone("done", "Mark a task as done", true),
		setDone("undone", "Mark a task as not done", false),
		rm)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// printTasks writes a table of tasks. Inside one list the extra column says
// who each task is for; across lists it says which list it is in.
func printTasks(cmd *cobra.Command, ts []store.Task, withList bool) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	head := "ID\tDONE\tTITLE\tDUE\tFOR\tDESCRIPTION"
	if withList {
		head = "ID\tDONE\tTITLE\tDUE\tLIST\tDESCRIPTION"
	}
	fmt.Fprintln(w, head)
	for _, t := range ts {
		mark := "[ ]"
		if t.Done {
			mark = "[x]"
		}
		who := t.Assignee
		if withList {
			who = t.ListName
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", t.ID, mark, t.Title, t.DueDate, who, t.Description)
	}
	return w.Flush()
}

// findList fetches the lists visible to the caller and returns the one with the given ID.
func findList(client *resty.Client, check func(*resty.Response, error) error, idArg string) (store.List, error) {
	id, err := parseID(idArg)
	if err != nil {
		return store.List{}, err
	}
	var ls []store.List
	if err := check(client.R().SetResult(&ls).Get("/api/lists")); err != nil {
		return store.List{}, err
	}
	for _, l := range ls {
		if strconv.FormatInt(l.ID, 10) == id {
			return l, nil
		}
	}
	return store.List{}, fmt.Errorf("list %s not found (are you signed in? use --token)", id)
}

func parseID(s string) (string, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return "", fmt.Errorf("invalid id %q", s)
	}
	return s, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
