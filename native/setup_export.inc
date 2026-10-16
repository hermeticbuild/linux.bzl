/* SPDX-License-Identifier: GPL-3.0-or-later
 * Export exactly the file/command variables passed to native sub-Makes.
 * Environment-origin variables are deliberately excluded.
 */
static void
setup_json_string (FILE *out, const char *s)
{
  fputc ('"', out);
  for (; *s; ++s)
    {
      unsigned char c = *s;
      if (c == '"' || c == '\\') { fputc ('\\', out); fputc (c, out); }
      else if (c < 32) fprintf (out, "\\u%04x", c);
      else fputc (c, out);
    }
  fputc ('"', out);
}

void
make_ast_export_setup (const char *path)
{
  char **env = target_environment (NULL, 0), **entry;
  FILE *out = fopen (path, "w");
  int first = 1;
  if (!out) pfatal_with_name (path);
  fputc ('{', out);
  for (entry = env; *entry; ++entry)
    {
      char *equal = strchr (*entry, '=');
      struct variable *v = equal ? lookup_variable (*entry, equal - *entry) : NULL;
      if (v && (v->origin == o_file || v->origin == o_override || v->origin == o_command)
          && !strneq (*entry, "MAKE", 4))
        {
          *equal = '\0';
          if (!first) fputc (',', out);
          first = 0;
          setup_json_string (out, *entry);
          fputc (':', out);
          setup_json_string (out, equal + 1);
        }
      free (*entry);
    }
  free (env);
  fputs ("}\n", out);
  fclose (out);
}
