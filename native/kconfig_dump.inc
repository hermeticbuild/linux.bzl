/* SPDX-License-Identifier: GPL-2.0-only */
/* Prototype frontend linked against the checkout's own Kconfig parser. */
#include <stdio.h>
#include <stdint.h>
#include "lkc.h"
#include <list.h>

static unsigned sequence;
static int first = 1;

static void string(const char *s)
{
    const unsigned char *p = (const unsigned char *)(s ? s : "");
    putchar('"');
    for (; *p; p++) {
        switch (*p) {
        case '"': fputs("\\\"", stdout); break;
        case '\\': fputs("\\\\", stdout); break;
        case '\n': fputs("\\n", stdout); break;
        case '\r': fputs("\\r", stdout); break;
        case '\t': fputs("\\t", stdout); break;
        default:
            if (*p < 32) printf("\\u%04x", *p);
            else putchar(*p);
        }
    }
    putchar('"');
}

static void expr_text(void *data, struct symbol *sym, const char *text)
{
    struct gstr *s = data;
    if (sym && (sym->flags & SYMBOL_CONST) && sym != &symbol_yes &&
        sym != &symbol_no && sym != &symbol_mod) {
        str_append(s, "\"");
        for (const char *p = text; *p; p++) {
            if (*p == '\\' || *p == '"') str_append(s, "\\");
            str_printf(s, "%c", *p);
        }
        str_append(s, "\"");
    } else {
        str_append(s, text);
    }
}

static void expression(struct expr *e)
{
    struct gstr s = str_new();
    if (e && e->type == E_RANGE) {
        expr_text(&s, e->left.sym, e->left.sym->name);
        str_append(&s, " ");
        expr_text(&s, e->right.sym, e->right.sym->name);
    } else if (e) expr_print(e, expr_text, &s, E_NONE);
    else str_append(&s, "y");
    string(str_get(&s));
    str_free(&s);
}

/* Postfix instructions preserve native expression types without re-parsing. */
static void code_symbol(struct symbol *sym)
{
    if (!sym->name) {
        struct menu *m = list_first_entry(&sym->menus, struct menu, link);
        printf("[\"sym\",\"choice:%u\"],", (unsigned)(uintptr_t)m->data);
    } else {
        printf("[\"%s\",", sym->flags & SYMBOL_CONST ? "lit" : "sym");
        string(sym->name);
        if (sym->flags & SYMBOL_CONST) {
            putchar(','); string(sym_type_name(sym->type));
        }
        fputs("],", stdout);
    }
}

static void code_expr(struct expr *e)
{
    const char *op;
    if (!e) return;
    switch (e->type) {
    case E_SYMBOL: code_symbol(e->left.sym); return;
    case E_NOT:
        code_expr(e->left.expr); fputs("[\"not\"],", stdout); return;
    case E_AND: case E_OR:
        code_expr(e->left.expr); code_expr(e->right.expr);
        op = e->type == E_AND ? "and" : "or"; break;
    case E_EQUAL: case E_UNEQUAL: case E_LTH: case E_LEQ: case E_GTH: case E_GEQ: case E_RANGE:
        code_symbol(e->left.sym); code_symbol(e->right.sym);
        op = e->type == E_EQUAL ? "eq" : e->type == E_UNEQUAL ? "ne" :
             e->type == E_LTH ? "lt" : e->type == E_LEQ ? "le" :
             e->type == E_GTH ? "gt" : e->type == E_GEQ ? "ge" : "range";
        break;
    default: fprintf(stderr, "Unsupported expression type %d\n", e->type); exit(1);
    }
    printf("[\"%s\"],", op);
}

static void bytecode(struct expr *e, const char *fallback)
{
    putchar('[');
    if (e) code_expr(e);
    else printf("[\"lit\",\"%s\",\"tristate\"],", fallback);
    /* A trailing no-op avoids special cases in the streaming writer. */
    fputs("[\"end\"]]", stdout);
}

static void reference(void *data, struct symbol *sym, const char *text)
{
    (void)data;
    (void)text;
    if (sym && sym->name && !(sym->flags & SYMBOL_CONST)) {
        string(sym->name);
        putchar(',');
    }
}

static void assign_ids(struct menu *menu)
{
    for (; menu; menu = menu->next) {
        if (menu->sym) menu->data = (void *)(uintptr_t)++sequence;
        if (menu->list) assign_ids(menu->list);
    }
}

static void walk(struct menu *menu, unsigned parent_choice)
{
    for (; menu; menu = menu->next) {
        struct symbol *sym = menu->sym;
        unsigned choice = parent_choice;
        if (sym) {
            unsigned id = (uintptr_t)menu->data;
            struct menu *group = sym_get_choice_menu(sym);
            struct property *p;
            if (!first) putchar(',');
            first = 0;
            printf("{\"order\":%u,\"parent_choice\":%u,\"choice\":%s,\"symbol\":",
                   id, parent_choice, sym_is_choice(sym) ? "true" : "false");
            string(sym->name);
            printf(",\"choice_group\":%u", sym_is_choice(sym) ? id : group ? (unsigned)(uintptr_t)group->data : 0);
            fputs(",\"type\":", stdout); string(sym_type_name(sym->type));
            fputs(",\"source\":", stdout); string(menu->filename);
            printf(",\"line\":%d,\"modules\":%s,\"transitional\":%s,\"depends_on\":",
                   menu->lineno, sym == modules_sym ? "true" : "false",
                   sym->flags & SYMBOL_TRANS ? "true" : "false");
            expression(menu->dep);
            fputs(",\"depends_code\":", stdout); bytecode(menu->dep, "y");
            fputs(",\"direct_code\":", stdout); bytecode(sym->dir_dep.expr, "y");
            fputs(",\"reverse_code\":", stdout); bytecode(sym->rev_dep.expr, "n");
            fputs(",\"implied_code\":", stdout); bytecode(sym->implied.expr, "n");
            fputs(",\"properties\":[", stdout);
            int prop_first = 1;
            for (p = sym->prop; p; p = p->next) {
                if (p->menu != menu) continue;
                if (!prop_first) putchar(',');
                prop_first = 0;
                fputs("{\"kind\":", stdout); string(prop_get_type_name(p->type));
                fputs(",\"text\":", stdout); string(p->text);
                fputs(",\"value\":", stdout); expression(p->expr);
                fputs(",\"condition\":", stdout); expression(p->visible.expr);
                fputs(",\"value_code\":", stdout); bytecode(p->expr, "y");
                fputs(",\"condition_code\":", stdout); bytecode(p->visible.expr, "y");
                putchar('}');
            }
            fputs("],\"references\":[", stdout);
            expr_print(menu->dep, reference, NULL, 0);
            for (p = sym->prop; p; p = p->next) {
                if (p->menu != menu) continue;
                expr_print(p->expr, reference, NULL, 0);
                expr_print(p->visible.expr, reference, NULL, 0);
            }
            fputs("\"\"],\"resolution_refs\":[", stdout);
            /* Match sym_check_sym_deps(), including reverse select/imply edges.
             * Choice members are collapsed into one resolution unit by the exporter.
             */
            expr_print(sym->dir_dep.expr, reference, NULL, 0);
            expr_print(sym->rev_dep.expr, reference, NULL, 0);
            expr_print(sym->implied.expr, reference, NULL, 0);
            for (p = sym->prop; p; p = p->next) {
                if (p->type == P_SELECT || p->type == P_IMPLY) continue;
                expr_print(p->visible.expr, reference, NULL, 0);
                if ((p->type == P_DEFAULT && !sym_is_choice(sym)) || p->type == P_RANGE)
                    expr_print(p->expr, reference, NULL, 0);
            }
            if (sym->type == S_TRISTATE)
                reference(NULL, modules_sym, NULL);
            fputs("\"\"]}", stdout);
            if (sym_is_choice(sym)) choice = id;
        }
        if (menu->list) walk(menu->list, choice);
    }
}
