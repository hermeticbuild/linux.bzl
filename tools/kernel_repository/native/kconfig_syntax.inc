/* File-local syntax export. Source statements are entries, not file reads. */
static void syntax_ids(struct menu *menu)
{
    for (; menu; menu = menu->next) {
        menu->data = (void *)(uintptr_t)++sequence;
        if (menu->list) syntax_ids(menu->list);
    }
}

static void syntax_walk(struct menu *menu, unsigned parent)
{
    for (; menu; menu = menu->next) {
        struct symbol *sym = menu->sym;
        unsigned id = (uintptr_t)menu->data;
        if (!first) putchar(',');
        first = 0;
        printf("{\"id\":%u,\"parent\":%u,\"kind\":", id, parent);
        string(menu->help && menu->type == M_COMMENT ? "source" :
               menu->type == M_CHOICE ? "choice" : sym ? "config" : "scope");
        fputs(",\"symbol\":", stdout); string(sym ? sym->name : "");
        fputs(",\"type\":", stdout); string(sym ? sym_type_name(sym->type) : "unknown");
        fputs(",\"source\":", stdout); string(menu->filename);
        printf(",\"line\":%d,\"modules\":%s,\"transitional\":%s", menu->lineno,
               sym && sym == modules_sym ? "true" : "false",
               sym && (sym->flags & SYMBOL_TRANS) ? "true" : "false");
        fputs(",\"path\":", stdout); string(menu->type == M_COMMENT ? menu->help : "");
        fputs(",\"depends\":", stdout); bytecode(menu->dep, "y");
        fputs(",\"visibility\":", stdout); bytecode(menu->visibility, "y");
        fputs(",\"properties\":[", stdout);
        int prop_first = 1;
        for (struct property *p = sym ? sym->prop : NULL; p; p = p->next) {
            if (p->menu != menu) continue;
            if (!prop_first) putchar(',');
            prop_first = 0;
            fputs("{\"kind\":", stdout); string(prop_get_type_name(p->type));
            fputs(",\"text\":", stdout); string(p->text);
            fputs(",\"value\":", stdout); bytecode(p->expr, "y");
            fputs(",\"when\":", stdout); bytecode(p->visible.expr, "y");
            putchar('}');
        }
        fputs("]}", stdout);
        if (menu->list) syntax_walk(menu->list, id);
    }
}

int main(int argc, char **argv)
{
    if (argc != 2) return 2;
    conf_parse(argv[1]);
    syntax_ids(rootmenu.list);
    putchar('[');
    syntax_walk(rootmenu.list, 0);
    puts("]");
    return 0;
}
