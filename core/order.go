package core

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/OpenBazaar/openbazaar-go/ipfs"
	"github.com/OpenBazaar/openbazaar-go/pb"
	"github.com/OpenBazaar/spvwallet"
	"github.com/btcsuite/btcd/btcec"
	hd "github.com/btcsuite/btcutil/hdkeychain"
	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/timestamp"
	peer "gx/ipfs/QmRBqJF7hb8ZSpRcMwUt8hNhydWcxGEhtk81HKq6oUwKvs/go-libp2p-peer"
	"gx/ipfs/QmT6n4mspWYEya864BhCUJEgyxiRfmiSY9ruQwTUNpRKaM/protobuf/proto"
	crypto "gx/ipfs/QmUWER4r4qMvaCnX5zREcfyiWN7cXN9g3a7fkRqNz8qWPP/go-libp2p-crypto"
	mh "gx/ipfs/QmYf7ng2hG5XBtJA3tN34DQ2GUN5HNksEw1rLDkmr6vGku/go-multihash"
	"strings"
	"time"
)

type option struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type shippingOption struct {
	Name    string `json:"name"`
	Service string `json:"service"`
}

type item struct {
	ListingHash string         `json:"listingHash"`
	Quantity    int            `json:"quantity"`
	Options     []option       `json:"options"`
	Shipping    shippingOption `json:"shipping"`
	Memo        string         `json:"memo"`
}

type PurchaseData struct {
	ShipTo       string `json:"shipTo"`
	Address      string `json:"address"`
	City         string `json:"city"`
	State        string `json:"state"`
	PostalCode   string `json:"postalCode"`
	CountryCode  string `json:"countryCode"`
	AddressNotes string `json:"addressNotes"`
	Moderator    string `json:"moderator"`
	Items        []item `json:"items"`
}

func (n *OpenBazaarNode) Purchase(data *PurchaseData) error {
	// TODO: validate the purchase data is formatted properly
	contract := new(pb.RicardianContract)
	order := new(pb.Order)
	order.RefundAddress = n.Wallet.CurrentAddress(spvwallet.EXTERNAL).EncodeAddress()

	shipping := new(pb.Order_Shipping)
	shipping.ShipTo = data.ShipTo
	shipping.Address = data.Address
	shipping.City = data.City
	shipping.State = data.State
	shipping.PostalCode = data.PostalCode
	shipping.Country = pb.CountryCode(pb.CountryCode_value[data.CountryCode])
	order.Shipping = shipping

	profile, err := n.GetProfile()
	if err != nil {
		return err
	}

	id := new(pb.ID)
	id.BlockchainID = profile.Handle
	id.Guid = n.IpfsNode.Identity.Pretty()
	pubkey, err := n.IpfsNode.PrivateKey.GetPublic().Bytes()
	if err != nil {
		return err
	}
	keys := new(pb.ID_Pubkeys)
	keys.Guid = pubkey
	ecPubKey, err := n.Wallet.MasterPublicKey().ECPubKey()
	if err != nil {
		return err
	}
	keys.Bitcoin = ecPubKey.SerializeCompressed()
	id.Pubkeys = keys
	order.BuyerID = id

	ts := new(timestamp.Timestamp)
	ts.Seconds = time.Now().Unix()
	ts.Nanos = 0
	order.Timestamp = ts

	for _, item := range data.Items {
		i := new(pb.Order_Item)

		// Let's fetch the listing, should be cached.
		b, err := ipfs.Cat(n.Context, item.ListingHash)
		if err != nil {
			return err
		}
		rc := new(pb.RicardianContract)
		err = jsonpb.UnmarshalString(string(b), rc)
		if err != nil {
			return err
		}
		if err := validate(rc.VendorListings[0]); err != nil {
			return fmt.Errorf("Listing failed to validate, reason: %q", err.Error())
		}
		if err := verifySignaturesOnListing(rc); err != nil {
			return err
		}

		// validate the selected options
		var userOptions []option
		var listingOptions []string
		for _, opt := range rc.VendorListings[0].Item.Options {
			listingOptions = append(listingOptions, opt.Name)
		}
		for _, uopt := range item.Options {
			userOptions = append(userOptions, uopt)
		}
		for _, checkOpt := range userOptions {
			for _, o := range rc.VendorListings[0].Item.Options {
				if strings.ToLower(o.Name) == strings.ToLower(checkOpt.Name) {
					var validVariant bool = false
					for _, v := range o.Variants {
						if strings.ToLower(v.Name) == strings.ToLower(checkOpt.Value) {
							validVariant = true
						}
					}
					if validVariant == false {
						return errors.New("Selected vairant not in listing")
					}
				}
			}
		check:
			for i, lopt := range listingOptions {
				if strings.ToLower(checkOpt.Name) == strings.ToLower(lopt) {
					listingOptions = append(listingOptions[:i], listingOptions[i+1:]...)
					continue check
				}
			}
		}
		if len(listingOptions) > 0 {
			return errors.New("Not all options were selected")
		}

		contract.VendorListings = append(contract.VendorListings, rc.VendorListings[0])
		contract.Signatures = append(contract.Signatures, rc.Signatures[0])
		ser, err := proto.Marshal(rc.VendorListings[0])
		if err != nil {
			return err
		}
		h := sha256.Sum256(ser)
		encoded, err := mh.Encode(h[:], mh.SHA2_256)
		if err != nil {
			return err
		}
		listingMH, err := mh.Cast(encoded)
		if err != nil {
			return err
		}
		i.ListingHash = listingMH.B58String()
		i.Quantity = uint32(item.Quantity)

		for _, option := range item.Options {
			o := new(pb.Order_Item_Option)
			o.Name = option.Name
			o.Value = option.Value
			i.Options = append(i.Options, o)
		}
		so := new(pb.Order_Item_ShippingOption)
		so.Title = item.Shipping.Name
		so.Service = item.Shipping.Service
		i.Memo = item.Memo
		order.Items = append(order.Items, i)
	}

	contract.BuyerOrder = order

	// Add payment data and send to vendor
	if data.Moderator != "" {

	} else { // direct payment
		payment := new(pb.Order_Payment)
		payment.Method = pb.Order_Payment_ADDRESS_REQUEST
		contract.BuyerOrder.Payment = payment

		// Send to order vendor and request a payment address
		resp, err := n.SendOrder(contract.VendorListings[0].VendorID.Guid, contract)
		if err != nil { // Vendor offline
			// Change payment code to direct
			payment.Method = pb.Order_Payment_DIRECT

			// Generated an payment address using the first child key derived from the vendor's
			// masterPubKey and a random chaincode.
			chaincode := make([]byte, 32)
			_, err := rand.Read(chaincode)
			if err != nil {
				return err
			}
			parentFP := []byte{0x00, 0x00, 0x00, 0x00}
			hdKey := hd.NewExtendedKey(
				n.Wallet.Params().HDPublicKeyID[:],
				contract.VendorListings[0].VendorID.Pubkeys.Bitcoin,
				chaincode,
				parentFP,
				0,
				0,
				false)

			childKey, err := hdKey.Child(1)
			if err != nil {
				return err
			}
			addr, err := childKey.Address(n.Wallet.Params())
			if err != nil {
				return err
			}
			// TODO: calculate the amount to be paid
			payment.Address = addr.EncodeAddress()
			payment.Chaincode = hex.EncodeToString(chaincode)

			// Send using offline messaging
			log.Warningf("Vendor %s is offline, sending offline order message", contract.VendorListings[0].VendorID.Guid)
			peerId, err := peer.IDB58Decode(contract.VendorListings[0].VendorID.Guid)
			if err != nil {
				return err
			}
			any, err := ptypes.MarshalAny(contract)
			if err != nil {
				return err
			}
			m := pb.Message{
				MessageType: pb.Message_ORDER,
				Payload:     any,
			}
			err = n.SendOfflineMessage(peerId, &m)
			if err != nil {
				return err
			}
		} else { // Vendor responded
			if resp.MessageType != pb.Message_ORDER_CONFIRMATION {
				return errors.New("Vendor responded to the order with an incorrect message type")
			}
		}
	}
	return nil
}

func verifySignaturesOnListing(contract *pb.RicardianContract) error {
	for n, listing := range contract.VendorListings {
		guidPubkeyBytes := listing.VendorID.Pubkeys.Guid
		bitcoinPubkeyBytes := listing.VendorID.Pubkeys.Bitcoin
		guid := listing.VendorID.Guid
		ser, err := proto.Marshal(listing)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(ser)
		guidPubkey, err := crypto.UnmarshalPublicKey(guidPubkeyBytes)
		if err != nil {
			return err
		}
		bitcoinPubkey, err := btcec.ParsePubKey(bitcoinPubkeyBytes, btcec.S256())
		if err != nil {
			return err
		}
		var guidSig []byte
		var bitcoinSig *btcec.Signature
		sig := contract.Signatures[n]
		if sig.Section != pb.Signatures_LISTING {
			return errors.New("Contract does not contain listing signature")
		}
		guidSig = sig.Guid
		bitcoinSig, err = btcec.ParseSignature(sig.Bitcoin, btcec.S256())
		if err != nil {
			return err
		}

		valid, err := guidPubkey.Verify(ser, guidSig)
		if err != nil {
			return err
		}
		if !valid {
			return errors.New("Vendor's guid signature on contact failed to verify")
		}
		checkKeyHash, err := guidPubkey.Hash()
		if err != nil {
			return err
		}
		guidMH, err := mh.FromB58String(guid)
		if err != nil {
			return err
		}
		for i, b := range []byte(guidMH) {
			if b != checkKeyHash[i] {
				return errors.New("Public key in listing does not match reported vendor ID")
			}
		}
		valid = bitcoinSig.Verify(hash[:], bitcoinPubkey)
		if !valid {
			return errors.New("Vendor's bitcoin signature on contact failed to verify")
		}
	}
	return nil
}
