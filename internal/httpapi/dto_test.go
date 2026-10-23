package httpapi

import (
	"testing"

	"github.com/mailquay/mailquay/internal/httpapi/oapi"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

func TestMapAccountSpecialFolders(t *testing.T) {
	account := sqlcgen.Account{
		AutoDetectSpecialFolders: false,
		SpecialFolders:           []byte(`{"trash":"Deleted Items","sent":"Sent Items"}`),
	}
	mapped := mapAccount(account)
	if mapped.AutoDetectSpecialFolders == nil || *mapped.AutoDetectSpecialFolders {
		t.Fatal("auto detect flag not mapped")
	}
	if mapped.SpecialFolders == nil || mapped.SpecialFolders.Trash == nil || *mapped.SpecialFolders.Trash != "Deleted Items" {
		t.Fatalf("special folders = %+v", mapped.SpecialFolders)
	}
	if mapped.SpecialFolders.Drafts != nil {
		t.Fatal("unset drafts folder should stay nil")
	}
}

func TestAccountInputSpecialFolders(t *testing.T) {
	auto := false
	trash := " Trash "
	body := oapi.AccountInput{
		AutoDetectSpecialFolders: &auto,
		SpecialFolders:           &oapi.SpecialFolders{Trash: &trash},
	}
	in := accountInput(body)
	if in.AutoDetectSpecialFolders == nil || *in.AutoDetectSpecialFolders {
		t.Fatal("auto detect flag not copied")
	}
	if in.SpecialFolders["trash"] != "Trash" {
		t.Fatalf("special folders = %v", in.SpecialFolders)
	}
}
